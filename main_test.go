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
  "net/http"
  "net/http/httptest"
  "testing"

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
}

func TestHandlePostMissingRoom(t *testing.T) {
  handler := &Handler{}
  body := SFURequest{
    Room:        "",
    OpenIDToken: OpenIDTokenType{AccessToken: "token", MatrixServerName: "server"},
    DeviceID:    "device",
  }
  jsonBody, _ := json.Marshal(body)

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
