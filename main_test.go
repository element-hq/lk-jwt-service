// Copyright 2025 New Vector Ltd

// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.

// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/matrix-org/gomatrix"
)

func TestHealthcheck(t *testing.T) {
  handler := &Handler{}
  req, err := http.NewRequest("GET", "/healthz", nil)
  if err != nil {
    t.Fatal(err)
  }

  rr := httptest.NewRecorder()
  handler.prepareMux().ServeHTTP(rr, req)

  if status := rr.Code; status != http.StatusOK {
    t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
  }
}

func TestHandleOptions(t *testing.T) {
  handler := &Handler{}
  req, err := http.NewRequest("OPTIONS", "/sfu/get", nil)
  if err != nil {
    t.Fatal(err)
  }

  rr := httptest.NewRecorder()
  handler.prepareMux().ServeHTTP(rr, req)

  if status := rr.Code; status != http.StatusOK {
    t.Errorf("handler returned wrong status code for OPTIONS: got %v want %v", status, http.StatusOK)
  }

  if accessControlAllowOrigin := rr.Header().Get("Access-Control-Allow-Origin"); accessControlAllowOrigin != "*" {
    t.Errorf("handler returned wrong Access-Control-Allow-Origin: got %v want %v", accessControlAllowOrigin, "*")
  }

  if accessControlAllowMethods := rr.Header().Get("Access-Control-Allow-Methods"); accessControlAllowMethods != "POST" {
    t.Errorf("handler returned wrong Access-Control-Allow-Methods: got %v want %v", accessControlAllowMethods, "POST")
  }
}

func TestHandlePostMissingParams(t *testing.T) {
  handler := &Handler{}

  testCases := []map[string]interface{} {
    {},
    {
      "room": "",
    },
  }

  for _, testCase := range testCases {
    jsonBody, _ := json.Marshal(testCase)

    req, err := http.NewRequest("POST", "/sfu/get", bytes.NewBuffer(jsonBody))
    if err != nil {
      t.Fatal(err)
    }
  
    rr := httptest.NewRecorder()
    handler.prepareMux().ServeHTTP(rr, req)
  
    if status := rr.Code; status != http.StatusBadRequest {
      t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusBadRequest)
    }
  
    var resp gomatrix.RespError
    err = json.NewDecoder(rr.Body).Decode(&resp)
    if err != nil {
      t.Errorf("failed to decode response body %v", err)
    }
  
    if resp.ErrCode != "M_BAD_JSON" {
      t.Errorf("unexpected error code: got %v want %v", resp.ErrCode, "M_BAD_JSON")
    }
  }
}

func TestHandlePost(t *testing.T) {
  handler := &Handler{
    secret: "testSecret",
    key: "testKey",
    lk_url: "wss://lk.local:8080/foo",
    skipVerifyTLS: true,
  }

  var matrixServerName = ""

  testServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    t.Log("Received request")
    // Inspect the request
    if r.URL.Path != "/_matrix/federation/v1/openid/userinfo" {
        t.Errorf("unexpected request path: got %v want %v", r.URL.Path, "/_matrix/federation/v1/openid/userinfo")
    }

    if accessToken := r.URL.Query().Get("access_token"); accessToken != "testAccessToken" {
      t.Errorf("unexpected access token: got %v want %v", accessToken, "testAccessToken")
    }

    // Mock response
    w.WriteHeader(http.StatusOK)
    w.Header().Set("Content-Type", "application/json")
    _, err := w.Write([]byte(fmt.Sprintf(`{"sub": "@user:%s"}`, matrixServerName)))
    if err != nil {
      t.Fatalf("failed to write response: %v", err)
    }
  }))
  defer testServer.Close()

  u, _ := url.Parse(testServer.URL)

  matrixServerName = u.Host

  testCase := map[string]interface{} {
    "room": "testRoom",
    "openid_token": map[string]interface{} {
      "access_token": "testAccessToken",
      "token_type": "testTokenType",
      "matrix_server_name": u.Host,
    },
    "device_id": "testDevice",
  }

  jsonBody, _ := json.Marshal(testCase)

  req, err := http.NewRequest("POST", "/sfu/get", bytes.NewBuffer(jsonBody))
  if err != nil {
    t.Fatal(err)
  }

  rr := httptest.NewRecorder()
  handler.prepareMux().ServeHTTP(rr, req)

  if status := rr.Code; status != http.StatusOK {
    t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
  }

  if contentType := rr.Header().Get("Content-Type"); contentType != "application/json" {
    t.Errorf("handler returned wrong Content-Type: got %v want %v", contentType, "application/json")
  }

  var resp SFUResponse
  err = json.NewDecoder(rr.Body).Decode(&resp)
  if err != nil {
    t.Errorf("failed to decode response body %v", err)
  }

  if resp.URL != "wss://lk.local:8080/foo" {
    t.Errorf("unexpected URL: got %v want %v", resp.URL, "wss://lk.local:8080/foo")
  }

  if resp.JWT == "" {
    t.Error("expected JWT to be non-empty")
  }

  // parse JWT checking the shared secret
  token, err := jwt.Parse(resp.JWT, func(token *jwt.Token) (interface{}, error) {
    return []byte(handler.secret), nil
  })

  if err != nil {
    t.Fatalf("failed to parse JWT: %v", err)
  }

  claims, ok := token.Claims.(jwt.MapClaims)
  
  if !ok || !token.Valid {
    t.Fatalf("failed to parse claims from JWT: %v", err)
  }

  if claims["sub"] != "@user:"+matrixServerName+":testDevice" {
    t.Errorf("unexpected sub: got %v want %v", claims["sub"], "@user:"+matrixServerName+":testDevice")
  }

  // should have permission for the room
  if claims["video"].(map[string]interface{})["room"] != "testRoom" {
    t.Errorf("unexpected room: got %v want %v", claims["room"], "testRoom")
  }
}

func TestGetJoinToken(t *testing.T) {
  apiKey := "testKey"
  apiSecret := "testSecret"
  room := "testRoom"
  identity := "testIdentity@example.com"

  token, err := getJoinToken(apiKey, apiSecret, room, identity)
  if err != nil {
    t.Fatalf("unexpected error: %v", err)
  }

  if token == "" {
    t.Error("expected token to be non-empty")
  }
}
