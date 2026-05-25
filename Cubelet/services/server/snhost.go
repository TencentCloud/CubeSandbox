// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/pathutil"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

var (
	ErrNotFound         = errors.New("resource not found")
	ErrMethodNotAllowed = errors.New("method not allowed")
)

const CodeOK = 200
const CodeBadRequest = 400
const CodeMethodNotAllowed = 405
const CodeNotFound = 404
const CodeInternalServerError = 500

type response struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	BDF     string `json:"bdf,omitempty"`
}

func newResponse(code int, message string, bdf ...string) *response {
	resp := &response{
		Code:    code,
		Message: message,
		BDF:     "",
	}
	if len(bdf) > 0 {
		resp.BDF = bdf[0]
	}
	return resp
}

type snhostProvider struct {
}

var snhostInstance *snhostProvider

type handlerFunc func(w http.ResponseWriter, r *http.Request)

func (h handlerFunc) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h(w, r)
}

func serveSNHost() map[string]http.Handler {
	snhostInstance = &snhostProvider{}
	handlers := make(map[string]http.Handler)

	handlers["/v1/snhost/bdf/by-ifname"] = handlerFunc(func(w http.ResponseWriter, r *http.Request) {
		snhostInstance.handleGetBDFByIfName(w, r)
	})
	handlers["/v1/snhost/bdf/by-uuid"] = handlerFunc(func(w http.ResponseWriter, r *http.Request) {
		snhostInstance.handleGetBDFByUUID(w, r)
	})

	return handlers
}

func (s *snhostProvider) writeJSONResponse(w http.ResponseWriter, statusCode int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		http.Error(w, fmt.Sprintf("Failed to encode response: %v", err), http.StatusInternalServerError)
		return
	}
}

func (s *snhostProvider) handleGetBDFByIfName(w http.ResponseWriter, r *http.Request) {
	defer utils.Recover()
	rt := &CubeLog.RequestTrace{
		Action:       "handleGetBDFByIfName",
		RequestID:    uuid.New().String(),
		Callee:       constants.SNHostID.ID(),
		CalleeAction: "handleGetBDFByIfName",
	}
	start := time.Now()

	defer func() {
		rt.Cost = time.Since(start)
		CubeLog.Trace(rt)
	}()

	if r.Method != http.MethodGet {
		resp := newResponse(CodeMethodNotAllowed, "Only GET requests are allowed for this endpoint.")
		s.writeJSONResponse(w, http.StatusMethodNotAllowed, resp)
		return
	}

	ifName := r.URL.Query().Get("ifname")
	if ifName == "" {
		resp := newResponse(CodeBadRequest, "Missing required parameter: ifname")
		s.writeJSONResponse(w, http.StatusBadRequest, resp)
		return
	}
	if err := pathutil.ValidateIfName(ifName); err != nil {
		resp := newResponse(CodeBadRequest, fmt.Sprintf("Invalid ifname parameter: %v", err))
		s.writeJSONResponse(w, http.StatusBadRequest, resp)
		return
	}

	pciID, err := s.getPCIIDByIfName(ifName)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			resp := newResponse(CodeNotFound, err.Error())
			s.writeJSONResponse(w, http.StatusNotFound, resp)
		} else {
			resp := newResponse(CodeInternalServerError, fmt.Sprintf("Internal server error: %v", err))
			s.writeJSONResponse(w, http.StatusInternalServerError, resp)
		}
		return
	}

	resp := newResponse(CodeOK, "ok", pciID)
	s.writeJSONResponse(w, http.StatusOK, resp)
}

func (s *snhostProvider) handleGetBDFByUUID(w http.ResponseWriter, r *http.Request) {
	defer utils.Recover()

	rt := &CubeLog.RequestTrace{
		Action:       "handleGetBDFByUUID",
		RequestID:    uuid.New().String(),
		Callee:       constants.SNHostID.ID(),
		CalleeAction: "handleGetBDFByUUID",
	}
	start := time.Now()

	defer func() {
		rt.Cost = time.Since(start)
		CubeLog.Trace(rt)
	}()

	if r.Method != http.MethodGet {
		resp := newResponse(CodeMethodNotAllowed, "Only GET requests are allowed for this endpoint.")
		s.writeJSONResponse(w, http.StatusMethodNotAllowed, resp)
		return
	}

	uuid := r.URL.Query().Get("uuid")
	if uuid == "" {
		resp := newResponse(CodeBadRequest, "Missing required parameter: uuid")
		s.writeJSONResponse(w, http.StatusBadRequest, resp)
		return
	}
	if err := pathutil.ValidateUUID(uuid); err != nil {
		resp := newResponse(CodeBadRequest, fmt.Sprintf("Invalid uuid parameter: %v", err))
		s.writeJSONResponse(w, http.StatusBadRequest, resp)
		return
	}

	pciID, err := s.getPCIIDByDiskUUID(uuid)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			resp := newResponse(CodeNotFound, err.Error())
			s.writeJSONResponse(w, http.StatusNotFound, resp)
		} else {
			resp := newResponse(CodeInternalServerError, fmt.Sprintf("Internal server error: %v", err))
			s.writeJSONResponse(w, http.StatusInternalServerError, resp)
		}
		return
	}

	resp := newResponse(CodeOK, "ok", pciID)
	s.writeJSONResponse(w, http.StatusOK, resp)
}

func (s *snhostProvider) getPCIIDByIfName(ifName string) (string, error) {
	stdout, stderr, err := utils.ExecBin(
		config.GetCommon().GetBDFByIfNameCmd,
		[]string{ifName},
		config.GetCommon().CommandTimeout,
	)
	if err != nil {
		notFoundMsg := fmt.Sprintf("tnic_client_net_get_bdf_by_ifname %s failed!", ifName)
		if strings.Contains(stderr, notFoundMsg) || strings.Contains(stdout, notFoundMsg) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("execution failed: %w (stdout: %s, stderr: %s)", err, stdout, stderr)
	}

	pciID := strings.TrimSpace(stdout)
	if pciID == "" {
		return "", ErrNotFound
	}
	return pciID, nil
}

func (s *snhostProvider) getPCIIDByDiskUUID(uuid string) (string, error) {
	stdout, stderr, err := utils.ExecBin(
		config.GetCommon().GetBDFByUuidCmd,
		[]string{uuid},
		config.GetCommon().CommandTimeout,
	)
	if err != nil {
		notFoundMsg := fmt.Sprintf("tnic_client_blk_query_bdf_by_uuid %s failed!", uuid)
		if strings.Contains(stderr, notFoundMsg) || strings.Contains(stdout, notFoundMsg) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("execution failed: %w (stdout: %s, stderr: %s)", err, stdout, stderr)
	}

	pciID := strings.TrimSpace(stdout)
	if pciID == "" {
		return "", ErrNotFound
	}
	return pciID, nil
}
