// Copyright (C) The Arvados Authors. All rights reserved.
//
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"git.curoverse.com/arvados.git/sdk/go/arvados"
	"git.curoverse.com/arvados.git/sdk/go/auth"
	"git.curoverse.com/arvados.git/sdk/go/httpserver"
	"git.curoverse.com/arvados.git/sdk/go/keepclient"
)

var pathPattern = `^/arvados/v1/%s(/([0-9a-z]{5})-%s-[0-9a-z]{15})?(.*)$`
var wfRe = regexp.MustCompile(fmt.Sprintf(pathPattern, "workflows", "7fd4e"))
var containersRe = regexp.MustCompile(fmt.Sprintf(pathPattern, "containers", "dz642"))
var containerRequestsRe = regexp.MustCompile(fmt.Sprintf(pathPattern, "container_requests", "xvhdp"))
var collectionRe = regexp.MustCompile(fmt.Sprintf(pathPattern, "collections", "4zz18"))
var collectionByPDHRe = regexp.MustCompile(`^/arvados/v1/collections/([0-9a-fA-F]{32}\+[0-9]+)+$`)

type genericFederatedRequestHandler struct {
	next    http.Handler
	handler *Handler
	matcher *regexp.Regexp
}

type collectionFederatedRequestHandler struct {
	next    http.Handler
	handler *Handler
}

func (h *Handler) remoteClusterRequest(remoteID string, w http.ResponseWriter, req *http.Request, filter ResponseFilter) {
	remote, ok := h.Cluster.RemoteClusters[remoteID]
	if !ok {
		httpserver.Error(w, "no proxy available for cluster "+remoteID, http.StatusNotFound)
		return
	}
	scheme := remote.Scheme
	if scheme == "" {
		scheme = "https"
	}
	err := h.saltAuthToken(req, remoteID)
	if err != nil {
		httpserver.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	urlOut := &url.URL{
		Scheme:   scheme,
		Host:     remote.Host,
		Path:     req.URL.Path,
		RawPath:  req.URL.RawPath,
		RawQuery: req.URL.RawQuery,
	}
	client := h.secureClient
	if remote.Insecure {
		client = h.insecureClient
	}
	h.proxy.Do(w, req, urlOut, client, filter)
}

// loadParamsFromForm expects a request with
// application/x-www-form-urlencoded body.  It parses the query, adds
// the query parameters to "params", and replaces the request body
// with a buffer holding the original body contents so it can be
// re-read by downstream proxy steps.
func loadParamsFromForm(req *http.Request, params url.Values) error {
	body, err := ioutil.ReadAll(req.Body)
	if err != nil {
		return err
	}
	req.Body = ioutil.NopCloser(bytes.NewBuffer(body))
	var v2 url.Values
	if v2, err = url.ParseQuery(string(body)); err != nil {
		return err
	}
	for k, v := range v2 {
		params[k] = append(params[k], v...)
	}
	return nil
}

// loadParamsFromForm expects a request with application/json body.
// It parses the body, populates "loadInto", and replaces the request
// body with a buffer holding the original body contents so it can be
// re-read by downstream proxy steps.
func loadParamsFromJson(req *http.Request, loadInto interface{}) error {
	var cl int64
	if req.ContentLength > 0 {
		cl = req.ContentLength
	}
	postBody := bytes.NewBuffer(make([]byte, 0, cl))
	defer req.Body.Close()

	rdr := io.TeeReader(req.Body, postBody)

	err := json.NewDecoder(rdr).Decode(loadInto)
	if err != nil {
		return err
	}
	req.Body = ioutil.NopCloser(postBody)
	return nil
}

type multiClusterQueryResponseCollector struct {
	mtx       sync.Mutex
	responses []interface{}
	errors    []error
	kind      string
}

func (c *multiClusterQueryResponseCollector) collectResponse(resp *http.Response,
	requestError error) (newResponse *http.Response, err error) {
	if requestError != nil {
		c.mtx.Lock()
		defer c.mtx.Unlock()
		c.errors = append(c.errors, requestError)
		return nil, nil
	}
	defer resp.Body.Close()
	loadInto := make(map[string]interface{})
	err = json.NewDecoder(resp.Body).Decode(&loadInto)

	c.mtx.Lock()
	defer c.mtx.Unlock()

	if err == nil {
		if resp.StatusCode != http.StatusOK {
			c.errors = append(c.errors, fmt.Errorf("error %v", loadInto["errors"]))
		} else {
			c.responses = append(c.responses, loadInto["items"].([]interface{})...)
			c.kind = loadInto["kind"].(string)
		}
	} else {
		c.errors = append(c.errors, err)
	}

	return nil, nil
}

func (h *genericFederatedRequestHandler) handleMultiClusterQuery(w http.ResponseWriter, req *http.Request,
	params url.Values, clusterId *string) bool {

	var filters [][]interface{}
	err := json.Unmarshal([]byte(params["filters"][0]), &filters)
	if err != nil {
		httpserver.Error(w, err.Error(), http.StatusBadRequest)
		return true
	}

	// Split the list of uuids by prefix
	queryClusters := make(map[string][]string)
	for _, f1 := range filters {
		if len(f1) != 3 {
			return false
		}
		lhs, ok := f1[0].(string)
		if ok && lhs == "uuid" {
			op, ok := f1[1].(string)
			if !ok {
				return false
			}

			if op == "in" {
				rhs, ok := f1[2].([]interface{})
				if ok {
					for _, i := range rhs {
						u := i.(string)
						*clusterId = u[0:5]
						queryClusters[u[0:5]] = append(queryClusters[u[0:5]], u)
					}
				}
			} else if op == "=" {
				u, ok := f1[2].(string)
				if ok {
					*clusterId = u[0:5]
					queryClusters[u[0:5]] = append(queryClusters[u[0:5]], u)
				}
			} else {
				return false
			}
		} else {
			return false
		}
	}

	if len(queryClusters) <= 1 {
		// Did not find a list query to search for uuids
		// across multiple clusters.
		return false
	}

	if !(len(params["count"]) == 1 && (params["count"][0] == `none` ||
		params["count"][0] == `"none"`)) {
		httpserver.Error(w, "Federated multi-object query must have 'count=none'", http.StatusBadRequest)
		return true
	}
	if len(params["limit"]) != 0 || len(params["offset"]) != 0 || len(params["order"]) != 0 {
		httpserver.Error(w, "Federated multi-object may not provide 'limit', 'offset' or 'order'.", http.StatusBadRequest)
		return true
	}

	wg := sync.WaitGroup{}

	// use channel as a semaphore to limit it to 4
	// parallel requests at a time
	sem := make(chan bool, 4)
	defer close(sem)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rc := multiClusterQueryResponseCollector{}
	for k, v := range queryClusters {
		// blocks until it can put a value into the
		// channel (which has a max queue capacity)
		sem <- true
		wg.Add(1)
		go func(k string, v []string) {
			defer func() {
				wg.Done()
				<-sem
			}()
			var remoteReq http.Request
			remoteReq.Header = req.Header
			remoteReq.Method = "POST"
			remoteReq.URL = &url.URL{Path: req.URL.Path}
			remoteParams := make(url.Values)
			remoteParams["_method"] = []string{"GET"}
			remoteParams["count"] = []string{"none"}
			if _, ok := params["select"]; ok {
				remoteParams["select"] = params["select"]
			}
			content, err := json.Marshal(v)
			if err != nil {
				rc.mtx.Lock()
				defer rc.mtx.Unlock()
				rc.errors = append(rc.errors, err)
				return
			}
			remoteParams["filters"] = []string{fmt.Sprintf(`[["uuid", "in", %s]]`, content)}
			enc := remoteParams.Encode()
			remoteReq.Body = ioutil.NopCloser(bytes.NewBufferString(enc))

			if k == h.handler.Cluster.ClusterID {
				h.handler.localClusterRequest(w, &remoteReq,
					rc.collectResponse)
			} else {
				h.handler.remoteClusterRequest(k, w, &remoteReq,
					rc.collectResponse)
			}
		}(k, v)
	}
	wg.Wait()

	if len(rc.errors) > 0 {
		// parallel query
		var strerr []string
		for _, e := range rc.errors {
			strerr = append(strerr, e.Error())
		}
		httpserver.Errors(w, strerr, http.StatusBadGateway)
	} else {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		itemList := make(map[string]interface{})
		itemList["items"] = rc.responses
		itemList["kind"] = rc.kind
		json.NewEncoder(w).Encode(itemList)
	}

	return true
}

func (h *genericFederatedRequestHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	m := h.matcher.FindStringSubmatch(req.URL.Path)
	clusterId := ""

	if len(m) > 0 && m[2] != "" {
		clusterId = m[2]
	}

	// First, parse the query portion of the URL.
	var params url.Values
	var err error
	if params, err = url.ParseQuery(req.URL.RawQuery); err != nil {
		httpserver.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Next, if appropriate, merge in parameters from the form POST body.
	if req.Method == "POST" && req.Header.Get("Content-Type") == "application/x-www-form-urlencoded" {
		if err = loadParamsFromForm(req, params); err != nil {
			httpserver.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	// Check if the parameters have an explicit cluster_id
	if len(params["cluster_id"]) == 1 {
		clusterId = params["cluster_id"][0]
	}

	// Handle the POST-as-GET special case (workaround for large
	// GET requests that potentially exceed maximum URL length,
	// like multi-object queries where the filter has 100s of
	// items)
	effectiveMethod := req.Method
	if req.Method == "POST" && len(params["_method"]) == 1 {
		effectiveMethod = params["_method"][0]
	}

	if effectiveMethod == "GET" && clusterId == "" && len(params["filters"]) == 1 {
		if h.handleMultiClusterQuery(w, req, params, &clusterId) {
			return
		}
	}

	if clusterId == "" || clusterId == h.handler.Cluster.ClusterID {
		h.next.ServeHTTP(w, req)
	} else {
		h.handler.remoteClusterRequest(clusterId, w, req, nil)
	}
}

type rewriteSignaturesClusterId struct {
	clusterID  string
	expectHash string
}

func (rw rewriteSignaturesClusterId) rewriteSignatures(resp *http.Response, requestError error) (newResponse *http.Response, err error) {
	if requestError != nil {
		return resp, requestError
	}

	if resp.StatusCode != 200 {
		return resp, nil
	}

	originalBody := resp.Body
	defer originalBody.Close()

	var col arvados.Collection
	err = json.NewDecoder(resp.Body).Decode(&col)
	if err != nil {
		return nil, err
	}

	// rewriting signatures will make manifest text 5-10% bigger so calculate
	// capacity accordingly
	updatedManifest := bytes.NewBuffer(make([]byte, 0, int(float64(len(col.ManifestText))*1.1)))

	hasher := md5.New()
	mw := io.MultiWriter(hasher, updatedManifest)
	sz := 0

	scanner := bufio.NewScanner(strings.NewReader(col.ManifestText))
	scanner.Buffer(make([]byte, 1048576), len(col.ManifestText))
	for scanner.Scan() {
		line := scanner.Text()
		tokens := strings.Split(line, " ")
		if len(tokens) < 3 {
			return nil, fmt.Errorf("Invalid stream (<3 tokens): %q", line)
		}

		n, err := mw.Write([]byte(tokens[0]))
		if err != nil {
			return nil, fmt.Errorf("Error updating manifest: %v", err)
		}
		sz += n
		for _, token := range tokens[1:] {
			n, err = mw.Write([]byte(" "))
			if err != nil {
				return nil, fmt.Errorf("Error updating manifest: %v", err)
			}
			sz += n

			m := keepclient.SignedLocatorRe.FindStringSubmatch(token)
			if m != nil {
				// Rewrite the block signature to be a remote signature
				_, err = fmt.Fprintf(updatedManifest, "%s%s%s+R%s-%s%s", m[1], m[2], m[3], rw.clusterID, m[5][2:], m[8])
				if err != nil {
					return nil, fmt.Errorf("Error updating manifest: %v", err)
				}

				// for hash checking, ignore signatures
				n, err = fmt.Fprintf(hasher, "%s%s", m[1], m[2])
				if err != nil {
					return nil, fmt.Errorf("Error updating manifest: %v", err)
				}
				sz += n
			} else {
				n, err = mw.Write([]byte(token))
				if err != nil {
					return nil, fmt.Errorf("Error updating manifest: %v", err)
				}
				sz += n
			}
		}
		n, err = mw.Write([]byte("\n"))
		if err != nil {
			return nil, fmt.Errorf("Error updating manifest: %v", err)
		}
		sz += n
	}

	// Check that expected hash is consistent with
	// portable_data_hash field of the returned record
	if rw.expectHash == "" {
		rw.expectHash = col.PortableDataHash
	} else if rw.expectHash != col.PortableDataHash {
		return nil, fmt.Errorf("portable_data_hash %q on returned record did not match expected hash %q ", rw.expectHash, col.PortableDataHash)
	}

	// Certify that the computed hash of the manifest_text matches our expectation
	sum := hasher.Sum(nil)
	computedHash := fmt.Sprintf("%x+%v", sum, sz)
	if computedHash != rw.expectHash {
		return nil, fmt.Errorf("Computed manifest_text hash %q did not match expected hash %q", computedHash, rw.expectHash)
	}

	col.ManifestText = updatedManifest.String()

	newbody, err := json.Marshal(col)
	if err != nil {
		return nil, err
	}

	buf := bytes.NewBuffer(newbody)
	resp.Body = ioutil.NopCloser(buf)
	resp.ContentLength = int64(buf.Len())
	resp.Header.Set("Content-Length", fmt.Sprintf("%v", buf.Len()))

	return resp, nil
}

func filterLocalClusterResponse(resp *http.Response, requestError error) (newResponse *http.Response, err error) {
	if requestError != nil {
		return resp, requestError
	}

	if resp.StatusCode == 404 {
		// Suppress returning this result, because we want to
		// search the federation.
		return nil, nil
	}
	return resp, nil
}

type searchRemoteClusterForPDH struct {
	pdh           string
	remoteID      string
	mtx           *sync.Mutex
	sentResponse  *bool
	sharedContext *context.Context
	cancelFunc    func()
	errors        *[]string
	statusCode    *int
}

func (s *searchRemoteClusterForPDH) filterRemoteClusterResponse(resp *http.Response, requestError error) (newResponse *http.Response, err error) {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	if *s.sentResponse {
		// Another request already returned a response
		return nil, nil
	}

	if requestError != nil {
		*s.errors = append(*s.errors, fmt.Sprintf("Request error contacting %q: %v", s.remoteID, requestError))
		// Record the error and suppress response
		return nil, nil
	}

	if resp.StatusCode != 200 {
		// Suppress returning unsuccessful result.  Maybe
		// another request will find it.
		// TODO collect and return error responses.
		*s.errors = append(*s.errors, fmt.Sprintf("Response from %q: %v", s.remoteID, resp.Status))
		if resp.StatusCode != 404 {
			// Got a non-404 error response, convert into BadGateway
			*s.statusCode = http.StatusBadGateway
		}
		return nil, nil
	}

	s.mtx.Unlock()

	// This reads the response body.  We don't want to hold the
	// lock while doing this because other remote requests could
	// also have made it to this point, and we don't want a
	// slow response holding the lock to block a faster response
	// that is waiting on the lock.
	newResponse, err = rewriteSignaturesClusterId{s.remoteID, s.pdh}.rewriteSignatures(resp, nil)

	s.mtx.Lock()

	if *s.sentResponse {
		// Another request already returned a response
		return nil, nil
	}

	if err != nil {
		// Suppress returning unsuccessful result.  Maybe
		// another request will be successful.
		*s.errors = append(*s.errors, fmt.Sprintf("Error parsing response from %q: %v", s.remoteID, err))
		return nil, nil
	}

	// We have a successful response.  Suppress/cancel all the
	// other requests/responses.
	*s.sentResponse = true
	s.cancelFunc()

	return newResponse, nil
}

func (h *collectionFederatedRequestHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != "GET" {
		// Only handle GET requests right now
		h.next.ServeHTTP(w, req)
		return
	}

	m := collectionByPDHRe.FindStringSubmatch(req.URL.Path)
	if len(m) != 2 {
		// Not a collection PDH GET request
		m = collectionRe.FindStringSubmatch(req.URL.Path)
		clusterId := ""

		if len(m) > 0 {
			clusterId = m[2]
		}

		if clusterId != "" && clusterId != h.handler.Cluster.ClusterID {
			// request for remote collection by uuid
			h.handler.remoteClusterRequest(clusterId, w, req,
				rewriteSignaturesClusterId{clusterId, ""}.rewriteSignatures)
			return
		}
		// not a collection UUID request, or it is a request
		// for a local UUID, either way, continue down the
		// handler stack.
		h.next.ServeHTTP(w, req)
		return
	}

	// Request for collection by PDH.  Search the federation.

	// First, query the local cluster.
	if h.handler.localClusterRequest(w, req, filterLocalClusterResponse) {
		return
	}

	sharedContext, cancelFunc := context.WithCancel(req.Context())
	defer cancelFunc()
	req = req.WithContext(sharedContext)

	// Create a goroutine for each cluster in the
	// RemoteClusters map.  The first valid result gets
	// returned to the client.  When that happens, all
	// other outstanding requests are cancelled or
	// suppressed.
	sentResponse := false
	mtx := sync.Mutex{}
	wg := sync.WaitGroup{}
	var errors []string
	var errorCode int = 404

	// use channel as a semaphore to limit it to 4
	// parallel requests at a time
	sem := make(chan bool, 4)
	defer close(sem)
	for remoteID := range h.handler.Cluster.RemoteClusters {
		// blocks until it can put a value into the
		// channel (which has a max queue capacity)
		sem <- true
		if sentResponse {
			break
		}
		search := &searchRemoteClusterForPDH{m[1], remoteID, &mtx, &sentResponse,
			&sharedContext, cancelFunc, &errors, &errorCode}
		wg.Add(1)
		go func() {
			h.handler.remoteClusterRequest(search.remoteID, w, req, search.filterRemoteClusterResponse)
			wg.Done()
			<-sem
		}()
	}
	wg.Wait()

	if sentResponse {
		return
	}

	// No successful responses, so return the error
	httpserver.Errors(w, errors, errorCode)
}

func (h *Handler) setupProxyRemoteCluster(next http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/arvados/v1/workflows", &genericFederatedRequestHandler{next, h, wfRe})
	mux.Handle("/arvados/v1/workflows/", &genericFederatedRequestHandler{next, h, wfRe})
	mux.Handle("/arvados/v1/containers", &genericFederatedRequestHandler{next, h, containersRe})
	mux.Handle("/arvados/v1/containers/", &genericFederatedRequestHandler{next, h, containersRe})
	mux.Handle("/arvados/v1/container_requests", &genericFederatedRequestHandler{next, h, containerRequestsRe})
	mux.Handle("/arvados/v1/container_requests/", &genericFederatedRequestHandler{next, h, containerRequestsRe})
	mux.Handle("/arvados/v1/collections", next)
	mux.Handle("/arvados/v1/collections/", &collectionFederatedRequestHandler{next, h})
	mux.Handle("/", next)

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		parts := strings.Split(req.Header.Get("Authorization"), "/")
		alreadySalted := (len(parts) == 3 && parts[0] == "Bearer v2" && len(parts[2]) == 40)

		if alreadySalted ||
			strings.Index(req.Header.Get("Via"), "arvados-controller") != -1 {
			// The token is already salted, or this is a
			// request from another instance of
			// arvados-controller.  In either case, we
			// don't want to proxy this query, so just
			// continue down the instance handler stack.
			next.ServeHTTP(w, req)
			return
		}

		mux.ServeHTTP(w, req)
	})

	return mux
}

type CurrentUser struct {
	Authorization arvados.APIClientAuthorization
	UUID          string
}

func (h *Handler) validateAPItoken(req *http.Request, user *CurrentUser) error {
	db, err := h.db(req)
	if err != nil {
		return err
	}
	return db.QueryRowContext(req.Context(), `SELECT api_client_authorizations.uuid, users.uuid FROM api_client_authorizations JOIN users on api_client_authorizations.user_id=users.id WHERE api_token=$1 AND (expires_at IS NULL OR expires_at > current_timestamp) LIMIT 1`, user.Authorization.APIToken).Scan(&user.Authorization.UUID, &user.UUID)
}

// Extract the auth token supplied in req, and replace it with a
// salted token for the remote cluster.
func (h *Handler) saltAuthToken(req *http.Request, remote string) error {
	creds := auth.NewCredentials()
	creds.LoadTokensFromHTTPRequest(req)
	if len(creds.Tokens) == 0 && req.Header.Get("Content-Type") == "application/x-www-form-encoded" {
		// Override ParseForm's 10MiB limit by ensuring
		// req.Body is a *http.maxBytesReader.
		req.Body = http.MaxBytesReader(nil, req.Body, 1<<28) // 256MiB. TODO: use MaxRequestSize from discovery doc or config.
		if err := creds.LoadTokensFromHTTPRequestBody(req); err != nil {
			return err
		}
		// Replace req.Body with a buffer that re-encodes the
		// form without api_token, in case we end up
		// forwarding the request.
		if req.PostForm != nil {
			req.PostForm.Del("api_token")
		}
		req.Body = ioutil.NopCloser(bytes.NewBufferString(req.PostForm.Encode()))
	}
	if len(creds.Tokens) == 0 {
		return nil
	}
	token, err := auth.SaltToken(creds.Tokens[0], remote)
	if err == auth.ErrObsoleteToken {
		// If the token exists in our own database, salt it
		// for the remote. Otherwise, assume it was issued by
		// the remote, and pass it through unmodified.
		currentUser := CurrentUser{Authorization: arvados.APIClientAuthorization{APIToken: creds.Tokens[0]}}
		err = h.validateAPItoken(req, &currentUser)
		if err == sql.ErrNoRows {
			// Not ours; pass through unmodified.
			token = currentUser.Authorization.APIToken
		} else if err != nil {
			return err
		} else {
			// Found; make V2 version and salt it.
			token, err = auth.SaltToken(currentUser.Authorization.TokenV2(), remote)
			if err != nil {
				return err
			}
		}
	} else if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	// Remove api_token=... from the the query string, in case we
	// end up forwarding the request.
	if values, err := url.ParseQuery(req.URL.RawQuery); err != nil {
		return err
	} else if _, ok := values["api_token"]; ok {
		delete(values, "api_token")
		req.URL.RawQuery = values.Encode()
	}
	return nil
}
