// Copyright 2018 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hub

import (
	"context"
	"crypto"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/google/certificate-transparency-go/tls"
	"github.com/google/trillian"
	"github.com/google/trillian-examples/gossip/api"
	"github.com/google/trillian/monitoring"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	tcrypto "github.com/google/trillian/crypto"
)

const (
	contentTypeHeader    string = "Content-Type"
	contentTypeJSON      string = "application/json"
	defaultMaxGetEntries int64  = 1000
)

var (
	// Use an explicitly empty slice for empty proofs so it gets JSON-encoded as
	// '[]' rather than 'null'.
	emptyProof = make([][]byte, 0)
)

var (
	// Metrics are all per-hub (label "hubid"), but may also be
	// per-entrypoint (label "ep") or per-return-code (label "rc").
	once             sync.Once
	knownHubs        monitoring.Gauge     // hubid => value (always 1.0)
	lastSTHTimestamp monitoring.Gauge     // hubid => value
	lastSTHTreeSize  monitoring.Gauge     // hubid => value
	reqsCounter      monitoring.Counter   // hubid, ep => value
	rspsCounter      monitoring.Counter   // hubid, ep, rc => value
	rspLatency       monitoring.Histogram // hubid, ep, rc => value
)

// setupMetrics initializes all the exported metrics.
func setupMetrics(mf monitoring.MetricFactory) {
	knownHubs = mf.NewGauge("known_hubs", "Set to 1 for known hubs", "hubid")
	lastSTHTimestamp = mf.NewGauge("last_sth_timestamp", "Time of last STH in ms since epoch", "hubid")
	lastSTHTreeSize = mf.NewGauge("last_sth_treesize", "Size of tree at last STH", "hubid")
	reqsCounter = mf.NewCounter("http_reqs", "Number of requests", "hubid", "ep")
	rspsCounter = mf.NewCounter("http_rsps", "Number of responses", "hubid", "ep", "rc")
	rspLatency = mf.NewHistogram("http_latency", "Latency of responses in seconds", "hubid", "ep", "rc")
}

// PathHandlers maps from a path to the relevant AppHandler instance.
type PathHandlers map[string]AppHandler

// AppHandler holds a LogContext and a handler function that uses it, and is
// an implementation of the http.Handler interface.
type AppHandler struct {
	info    *hubInfo
	epPath  string
	handler func(context.Context, *hubInfo, http.ResponseWriter, *http.Request) (int, error)
	method  string // http.MethodGet or http.MethodPost
}

// ServeHTTP for an AppHandler invokes the underlying handler function but
// does additional common error and stats processing.
func (a AppHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	label0 := strconv.FormatInt(a.info.logID, 10)
	label1 := string(a.epPath)
	reqsCounter.Inc(label0, label1)
	var status int
	startTime := time.Now()
	defer func() {
		rspLatency.Observe(time.Since(startTime).Seconds(), label0, label1, strconv.Itoa(status))
	}()
	glog.V(2).Infof("%s: request %v %q => %s", a.info.hubPrefix, r.Method, r.URL, a.epPath)
	if r.Method != a.method {
		glog.Warningf("%s: %s wrong HTTP method: %v", a.info.hubPrefix, a.epPath, r.Method)
		sendHTTPError(w, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed: %s", r.Method))
		return
	}

	// For GET requests all params come as form encoded so we might as well parse them now.
	// POSTs will decode the raw request body as JSON later.
	if r.Method == http.MethodGet {
		if err := r.ParseForm(); err != nil {
			sendHTTPError(w, http.StatusBadRequest, fmt.Errorf("failed to parse form data: %v", err))
			return
		}
	}

	// Many/most of the handlers forward the request on to the Log RPC server; impose a deadline
	// on this onward request.
	ctx, cancel := context.WithTimeout(ctx, a.info.opts.Deadline)
	defer cancel()

	status, err := a.handler(ctx, a.info, w, r)
	glog.V(2).Infof("%s: %s <= status=%d", a.info.hubPrefix, a.epPath, status)
	rspsCounter.Inc(label0, label1, strconv.Itoa(status))
	if err != nil {
		glog.Warningf("%s: %s handler error: %v", a.info.hubPrefix, a.epPath, err)
		sendHTTPError(w, status, err)
		return
	}

	// Additional check, for consistency the handler must return an error for non-200 status
	if status != http.StatusOK {
		glog.Warningf("%s: %s handler non 200 without error: %d %v", a.info.hubPrefix, a.epPath, status, err)
		sendHTTPError(w, http.StatusInternalServerError, fmt.Errorf("http handler misbehaved, status: %d", status))
		return
	}
}

// logCryptoInfo holds information needed to check signatures generated by a source Log.
type logCryptoInfo struct {
	pubKeyData []byte
	pubKey     crypto.PublicKey
	hasher     crypto.Hash
}

// hubInfo holds information about a specific hub instance.
type hubInfo struct {
	// Instance-wide options
	opts InstanceOptions

	hubPrefix string
	logID     int64
	urlPrefix string
	rpcClient trillian.TrillianLogClient
	signer    crypto.Signer
	cryptoMap map[string]logCryptoInfo
}

// newHubInfo creates a new instance of hubInfo.
func newHubInfo(logID int64, prefix string, rpcClient trillian.TrillianLogClient, signer crypto.Signer, cryptoMap map[string]logCryptoInfo, opts InstanceOptions) *hubInfo {
	info := &hubInfo{
		opts:      opts,
		hubPrefix: fmt.Sprintf("%s{%d}", prefix, logID),
		logID:     logID,
		urlPrefix: prefix,
		rpcClient: rpcClient,
		signer:    signer,
		cryptoMap: cryptoMap,
	}
	once.Do(func() { setupMetrics(opts.MetricFactory) })
	knownHubs.Set(1.0, strconv.FormatInt(logID, 10))

	return info
}

// Handlers returns a map from URL paths (with the given prefix) and AppHandler instances
// to handle those entrypoints.
func (h *hubInfo) Handlers(prefix string) PathHandlers {
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	prefix = strings.TrimRight(prefix, "/")

	// Bind the hubInfo instance to give an appHandler instance for each entrypoint.
	return PathHandlers{
		prefix + api.PathPrefix + api.AddLogHeadPath:        AppHandler{info: h, handler: addLogHead, epPath: api.AddLogHeadPath, method: http.MethodPost},
		prefix + api.PathPrefix + api.GetSTHPath:            AppHandler{info: h, handler: getSTH, epPath: api.GetSTHPath, method: http.MethodGet},
		prefix + api.PathPrefix + api.GetSTHConsistencyPath: AppHandler{info: h, handler: getSTHConsistency, epPath: api.GetSTHConsistencyPath, method: http.MethodGet},
		prefix + api.PathPrefix + api.GetProofByHashPath:    AppHandler{info: h, handler: getProofByHash, epPath: api.GetProofByHashPath, method: http.MethodGet},
		prefix + api.PathPrefix + api.GetEntriesPath:        AppHandler{info: h, handler: getEntries, epPath: api.GetEntriesPath, method: http.MethodGet},
		prefix + api.PathPrefix + api.GetLogKeysPath:        AppHandler{info: h, handler: getLogKeys, epPath: api.GetLogKeysPath, method: http.MethodGet},
	}
}

func addLogHead(ctx context.Context, c *hubInfo, w http.ResponseWriter, r *http.Request) (int, error) {
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		glog.V(1).Infof("%s: Failed to read request body: %v", c.hubPrefix, err)
		return http.StatusBadRequest, fmt.Errorf("failed to read add-log-head body: %v", err)
	}

	var req api.AddLogHeadRequest
	if err := json.Unmarshal(body, &req); err != nil {
		glog.V(1).Infof("%s: Failed to parse request body: %v", c.hubPrefix, err)
		return http.StatusBadRequest, fmt.Errorf("failed to parse add-log-head body: %v", err)
	}

	cryptoInfo, ok := c.cryptoMap[req.SourceURL]
	if !ok {
		glog.V(1).Infof("%s: unknown source log %q", c.hubPrefix, req.SourceURL)
		return http.StatusNotFound, fmt.Errorf("unknown source log %q", req.SourceURL)
	}

	// Verify the signature.
	if err := tcrypto.Verify(cryptoInfo.pubKey, cryptoInfo.hasher, req.HeadData, req.Signature); err != nil {
		glog.V(1).Infof("%s: failed to validate signature from %q: %v", c.hubPrefix, req.SourceURL, err)
		return http.StatusBadRequest, fmt.Errorf("failed to validate signature from %q", req.SourceURL)
	}

	// Build a HubLeafEntry for the data
	hubLeaf := api.HubLeafEntry{
		SourceURL: []byte(req.SourceURL),
		HeadData:  req.HeadData,
		Signature: req.Signature,
	}
	leafData, err := tls.Marshal(&hubLeaf)
	if err != nil {
		glog.V(1).Infof("%s: failed to tls.Marshal hub leaf for %q: %v", c.hubPrefix, req.SourceURL, err)
		return http.StatusBadRequest, fmt.Errorf("failed to create hub leaf: %v", err)
	}
	identityHash := sha256.Sum256(leafData)

	// Get the current time in nanos since Unix epoch and use throughout.
	//timeNanos := uint64(time.Now().UnixNano())

	leaf := trillian.LogLeaf{
		LeafValue:        leafData,
		LeafIdentityHash: identityHash[:],
	}

	// Send the leaf on to the Log server.
	glog.V(2).Infof("%s: AddLogHead => grpc.QueueLeaves", c.hubPrefix)
	rsp, err := c.rpcClient.QueueLeaves(ctx, &trillian.QueueLeavesRequest{LogId: c.logID, Leaves: []*trillian.LogLeaf{&leaf}})
	glog.V(2).Infof("%s: AddLogHead <= grpc.QueueLeaves err=%v", c.hubPrefix, err)
	if err != nil {
		return c.toHTTPStatus(err), fmt.Errorf("backend QueueLeaves request failed: %v", err)
	}
	if rsp == nil {
		return http.StatusInternalServerError, errors.New("missing QueueLeaves response")
	}
	if len(rsp.QueuedLeaves) != 1 {
		return http.StatusInternalServerError, fmt.Errorf("unexpected QueueLeaves response leaf count: %d", len(rsp.QueuedLeaves))
	}
	//queuedLeaf := rsp.QueuedLeaves[0]

	// Always use the returned leaf as the basis for an signed gossip timestamp.
	// @@@@@@@  only have the timestamp in ExtraData?

	glog.V(3).Infof("%s: AddLogRoot <= SGT", c.hubPrefix)

	return http.StatusOK, nil
}

// GetLogRoot retrieves a signed log root for the given log.
func GetLogRoot(ctx context.Context, client trillian.TrillianLogClient, logID int64, prefix string) (*trillian.SignedLogRoot, error) {
	// Send request to the Log server.
	req := trillian.GetLatestSignedLogRootRequest{LogId: logID}
	glog.V(2).Infof("%s: GetLogRoot => grpc.GetLatestSignedLogRoot %+v", prefix, req)
	rsp, err := client.GetLatestSignedLogRoot(ctx, &req)
	glog.V(2).Infof("%s: GetLogRoot <= grpc.GetLatestSignedLogRoot err=%v", prefix, err)
	if err != nil {
		return nil, fmt.Errorf("backend GetLatestSignedLogRoot request failed: %v", err)
	}

	// Check over the response.
	slr := rsp.SignedLogRoot
	if slr == nil {
		return nil, errors.New("no log root returned")
	}
	glog.V(3).Infof("%s: GetLogRoot <= slr=%+v", prefix, slr)

	return slr, nil
}

func getSTH(ctx context.Context, c *hubInfo, w http.ResponseWriter, r *http.Request) (int, error) {
	slr, err := GetLogRoot(ctx, c.rpcClient, c.logID, c.hubPrefix)
	if err != nil {
		return http.StatusInternalServerError, err
	}

	// @@@ signed by trillian not personality??   does personality need to do signing, or can it rely on trillian backend?

	// Now build the final result object that will be marshaled to JSON
	jsonRsp := api.GetSTHResponse{HeadData: slr.LogRoot, Signature: slr.LogRootSignature}
	jsonData, err := json.Marshal(&jsonRsp)
	if err != nil {
		return http.StatusInternalServerError, fmt.Errorf("failed to marshal response: %v %v", jsonRsp, err)
	}

	w.Header().Set(contentTypeHeader, contentTypeJSON)
	_, err = w.Write(jsonData)
	if err != nil {
		// Probably too late for this as headers might have been written but we don't know for sure
		return http.StatusInternalServerError, fmt.Errorf("failed to write response data: %v", err)
	}

	return http.StatusOK, nil
}

func getSTHConsistency(ctx context.Context, c *hubInfo, w http.ResponseWriter, r *http.Request) (int, error) {
	first, second, err := parseGetSTHConsistencyRange(r)
	if err != nil {
		return http.StatusBadRequest, fmt.Errorf("failed to parse consistency range: %v", err)
	}
	var jsonRsp api.GetSTHConsistencyResponse
	if first != 0 {
		req := trillian.GetConsistencyProofRequest{LogId: c.logID, FirstTreeSize: first, SecondTreeSize: second}

		glog.V(2).Infof("%s: GetSTHConsistency(%d, %d) => grpc.GetConsistencyProof %+v", c.hubPrefix, first, second, req)
		rsp, err := c.rpcClient.GetConsistencyProof(ctx, &req)
		glog.V(2).Infof("%s: GetSTHConsistency <= grpc.GetConsistencyProof err=%v", c.hubPrefix, err)
		if err != nil {
			return c.toHTTPStatus(err), fmt.Errorf("backend GetConsistencyProof request failed: %v", err)
		}

		// We can get here with a tree size too small to satisfy the proof.
		if rsp.SignedLogRoot != nil && rsp.SignedLogRoot.TreeSize < second {
			return http.StatusBadRequest, fmt.Errorf("need tree size: %d for proof but only got: %d", second, rsp.SignedLogRoot.TreeSize)
		}

		if err := checkHashSizes(rsp.Proof.Hashes); err != nil {
			return http.StatusInternalServerError, fmt.Errorf("backend returned invalid proof %v: %v", rsp.Proof, err)
		}

		// We got a valid response from the server. Marshal it as JSON and return it to the client
		jsonRsp.Consistency = rsp.Proof.Hashes
		if jsonRsp.Consistency == nil {
			jsonRsp.Consistency = emptyProof
		}
	} else {
		glog.V(2).Infof("%s: GetSTHConsistency(%d, %d) starts from 0 so return empty proof", c.hubPrefix, first, second)
		jsonRsp.Consistency = emptyProof
	}

	w.Header().Set(contentTypeHeader, contentTypeJSON)
	jsonData, err := json.Marshal(&jsonRsp)
	if err != nil {
		return http.StatusInternalServerError, fmt.Errorf("failed to marshal get-sth-consistency resp: %v because %v", jsonRsp, err)
	}

	_, err = w.Write(jsonData)
	if err != nil {
		// Probably too late for this as headers might have been written but we don't know for sure
		return http.StatusInternalServerError, fmt.Errorf("failed to write get-sth-consistency resp: %v because %v", jsonRsp, err)
	}

	return http.StatusOK, nil
}

func parseGetSTHConsistencyRange(r *http.Request) (int64, int64, error) {
	firstVal := r.FormValue(api.GetSTHConsistencyFirst)
	secondVal := r.FormValue(api.GetSTHConsistencySecond)
	if firstVal == "" {
		return 0, 0, errors.New("parameter 'first' is required")
	}
	if secondVal == "" {
		return 0, 0, errors.New("parameter 'second' is required")
	}

	first, err := strconv.ParseInt(firstVal, 10, 64)
	if err != nil {
		return 0, 0, errors.New("parameter 'first' is malformed")
	}

	second, err := strconv.ParseInt(secondVal, 10, 64)
	if err != nil {
		return 0, 0, errors.New("parameter 'second' is malformed")
	}

	if first < 0 || second < 0 {
		return 0, 0, fmt.Errorf("first and second params cannot be <0: %d %d", first, second)
	}
	if second < first {
		return 0, 0, fmt.Errorf("invalid first, second params: %d %d", first, second)
	}

	return first, second, nil
}

func getProofByHash(ctx context.Context, c *hubInfo, w http.ResponseWriter, r *http.Request) (int, error) {
	// Accept any non empty hash that decodes from base64 and let the backend validate it further
	hash := r.FormValue(api.GetProofByHashArg)
	if len(hash) == 0 {
		return http.StatusBadRequest, errors.New("get-proof-by-hash: missing / empty hash param for get-proof-by-hash")
	}
	leafHash, err := base64.StdEncoding.DecodeString(hash)
	if err != nil {
		return http.StatusBadRequest, fmt.Errorf("get-proof-by-hash: invalid base64 hash: %v", err)
	}

	treeSize, err := strconv.ParseInt(r.FormValue(api.GetProofByHashSize), 10, 64)
	if err != nil || treeSize < 1 {
		return http.StatusBadRequest, fmt.Errorf("get-proof-by-hash: missing or invalid tree_size: %v", err)
	}

	req := trillian.GetInclusionProofByHashRequest{
		LogId:           c.logID,
		LeafHash:        leafHash,
		TreeSize:        treeSize,
		OrderBySequence: true,
	}
	rsp, err := c.rpcClient.GetInclusionProofByHash(ctx, &req)
	if err != nil {
		return c.toHTTPStatus(err), fmt.Errorf("backend GetInclusionProofByHash request failed: %v", err)
	}

	// We could fail to get the proof because the tree size that the server has
	// is not large enough.
	if rsp.SignedLogRoot != nil && rsp.SignedLogRoot.TreeSize < treeSize {
		return http.StatusNotFound, fmt.Errorf("log returned tree size: %d but we expected: %d", rsp.SignedLogRoot.TreeSize, treeSize)
	}

	// Additional sanity checks on the response.
	if len(rsp.Proof) == 0 {
		// The backend returns the STH even when there is no proof, so explicitly
		// map this to 4xx.
		return http.StatusNotFound, errors.New("backend did not return a proof")
	}
	if err := checkHashSizes(rsp.Proof[0].Hashes); err != nil {
		return http.StatusInternalServerError, fmt.Errorf("backend returned invalid proof %v: %v", rsp.Proof, err)
	}

	// All checks complete, marshal and return the response
	proofRsp := api.GetProofByHashResponse{
		LeafIndex: rsp.Proof[0].LeafIndex,
		AuditPath: rsp.Proof[0].Hashes,
	}
	if proofRsp.AuditPath == nil {
		proofRsp.AuditPath = emptyProof
	}

	jsonData, err := json.Marshal(&proofRsp)
	if err != nil {
		glog.Warningf("%s: Failed to marshal get-proof-by-hash resp: %v", c.hubPrefix, proofRsp)
		return http.StatusInternalServerError, fmt.Errorf("failed to marshal get-proof-by-hash resp: %v, error: %v", proofRsp, err)
	}

	w.Header().Set(contentTypeHeader, contentTypeJSON)
	_, err = w.Write(jsonData)
	if err != nil {
		// Probably too late for this as headers might have been written but we don't know for sure
		return http.StatusInternalServerError, fmt.Errorf("failed to write get-proof-by-hash resp: %v", proofRsp)
	}

	return http.StatusOK, nil
}

func getEntries(ctx context.Context, c *hubInfo, w http.ResponseWriter, r *http.Request) (int, error) {
	// The first job is to parse the params and make sure they're sensible. We just make
	// sure the range is valid. We don't do an extra roundtrip to get the current tree
	// size and prefer to let the backend handle this case
	maxRange := c.opts.MaxGetEntries
	if maxRange == 0 {
		maxRange = defaultMaxGetEntries
	}
	start, end, err := parseGetEntriesRange(r, maxRange)
	if err != nil {
		return http.StatusBadRequest, fmt.Errorf("bad range on get-entries request: %v", err)
	}

	// Now make a request to the backend to get the relevant leaves
	count := end + 1 - start
	req := trillian.GetLeavesByRangeRequest{
		LogId:      c.logID,
		StartIndex: start,
		Count:      count,
	}
	rsp, err := c.rpcClient.GetLeavesByRange(ctx, &req)
	if err != nil {
		return c.toHTTPStatus(err), fmt.Errorf("backend GetLeavesByRange request failed: %v", err)
	}
	if rsp.SignedLogRoot != nil && rsp.SignedLogRoot.TreeSize <= start {
		// If the returned tree is too small to contain any leaves return the 4xx explicitly here.
		return http.StatusBadRequest, fmt.Errorf("need tree size: %d to get leaves but only got: %d", rsp.SignedLogRoot.TreeSize, start)
	}
	// Do some sanity checks on the result.
	if len(rsp.Leaves) > int(count) {
		return http.StatusInternalServerError, fmt.Errorf("backend returned too many leaves: %d vs [%d,%d]", len(rsp.Leaves), start, end)
	}
	for i, leaf := range rsp.Leaves {
		if leaf.LeafIndex != start+int64(i) {
			return http.StatusInternalServerError, fmt.Errorf("backend returned unexpected leaf index: rsp.Leaves[%d].LeafIndex=%d for range [%d,%d]", i, leaf.LeafIndex, start, end)
		}
	}

	var jsonRsp api.GetEntriesResponse
	for _, leaf := range rsp.Leaves {
		var hubLeaf api.HubLeafEntry
		if rest, err := tls.Unmarshal(leaf.LeafValue, &hubLeaf); err != nil {
			return http.StatusInternalServerError, fmt.Errorf("%s: Failed to deserialize Merkle leaf from backend: %d", c.hubPrefix, leaf.LeafIndex)
		} else if len(rest) > 0 {
			return http.StatusInternalServerError, fmt.Errorf("%s: Trailing data after Merkle leaf from backend: %d", c.hubPrefix, leaf.LeafIndex)
		}
		jsonRsp.Entries = append(jsonRsp.Entries, api.LeafEntry{LeafData: leaf.LeafValue})
	}

	jsonData, err := json.Marshal(&jsonRsp)
	if err != nil {
		return http.StatusInternalServerError, fmt.Errorf("failed to marshal get-entries resp: %v because: %v", jsonRsp, err)
	}

	w.Header().Set(contentTypeHeader, contentTypeJSON)
	_, err = w.Write(jsonData)
	if err != nil {
		return http.StatusInternalServerError, fmt.Errorf("failed to write get-entries resp: %v because: %v", jsonRsp, err)
	}

	return http.StatusOK, nil
}

func parseGetEntriesRange(r *http.Request, maxRange int64) (int64, int64, error) {
	start, err := strconv.ParseInt(r.FormValue(api.GetEntriesStart), 10, 64)
	if err != nil {
		return 0, 0, err
	}

	end, err := strconv.ParseInt(r.FormValue(api.GetEntriesEnd), 10, 64)
	if err != nil {
		return 0, 0, err
	}

	if start < 0 || end < 0 {
		return 0, 0, fmt.Errorf("start (%d) and end (%d) parameters must be >= 0", start, end)
	}
	if start > end {
		return 0, 0, fmt.Errorf("start (%d) and end (%d) is not a valid range", start, end)
	}

	count := end - start + 1
	if count > maxRange {
		end = start + maxRange - 1
	}
	return start, end, nil
}

func getLogKeys(ctx context.Context, c *hubInfo, w http.ResponseWriter, r *http.Request) (int, error) {
	var jsonRsp api.GetLogKeysResponse

	for url, info := range c.cryptoMap {
		l := api.LogKey{URL: url, PubKey: info.pubKeyData}
		jsonRsp.Entries = append(jsonRsp.Entries, &l)
	}

	jsonData, err := json.Marshal(&jsonRsp)
	if err != nil {
		return http.StatusInternalServerError, fmt.Errorf("failed to marshal get-log-keys rsp: %v because: %v", jsonRsp, err)
	}

	w.Header().Set(contentTypeHeader, contentTypeJSON)
	_, err = w.Write(jsonData)
	if err != nil {
		// Probably too late for this as headers might have been written but we don't know for sure
		return http.StatusInternalServerError, fmt.Errorf("failed to write get-entries resp: %v because: %v", jsonRsp, err)
	}

	return http.StatusOK, nil
}

func sendHTTPError(w http.ResponseWriter, statusCode int, err error) {
	http.Error(w, fmt.Sprintf("%s\n%v", http.StatusText(statusCode), err), statusCode)
}

func checkHashSizes(path [][]byte) error {
	for i, node := range path {
		if len(node) != sha256.Size {
			return fmt.Errorf("proof[%d] is length %d, want %d", i, len(node), sha256.Size)
		}
	}
	return nil
}

func (h *hubInfo) toHTTPStatus(err error) int {
	if h.opts.ErrorMapper != nil {
		if status, ok := h.opts.ErrorMapper(err); ok {
			return status
		}
	}

	rpcStatus, ok := status.FromError(err)
	if !ok {
		return http.StatusInternalServerError
	}

	switch rpcStatus.Code() {
	case codes.OK:
		return http.StatusOK
	case codes.Canceled, codes.DeadlineExceeded:
		return http.StatusRequestTimeout
	case codes.InvalidArgument, codes.OutOfRange, codes.AlreadyExists:
		return http.StatusBadRequest
	case codes.NotFound:
		return http.StatusNotFound
	case codes.PermissionDenied, codes.ResourceExhausted:
		return http.StatusForbidden
	case codes.Unauthenticated:
		return http.StatusUnauthorized
	case codes.FailedPrecondition:
		return http.StatusPreconditionFailed
	case codes.Aborted:
		return http.StatusConflict
	case codes.Unimplemented:
		return http.StatusNotImplemented
	case codes.Unavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}
