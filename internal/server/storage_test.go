package server_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wnarutou/gitrieve/internal/config"
	"github.com/wnarutou/gitrieve/internal/db"
	server "github.com/wnarutou/gitrieve/internal/server"
	"github.com/wnarutou/gitrieve/internal/typedef"
)

func storageResponseCode(t *testing.T, resp *httptest.ResponseRecorder) int {
	t.Helper()
	var r struct {
		Code    int         `json:"code"`
		Data    interface{} `json:"data"`
		Message string      `json:"message"`
	}
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &r))
	return r.Code
}

func TestGetStorages(t *testing.T) {
	testDB, err := db.Initialize(":memory:")
	require.NoError(t, err)
	defer testDB.Close()

	cfg := &config.Config{
		Storage: []typedef.MultiStorage{
			{Storage: typedef.Storage{Name: "local", Type: "file", Path: "/tmp/archives"}},
		},
	}

	s := server.NewStorageTestServer(cfg, testDB)

	req, _ := http.NewRequest("GET", "/api/storage", nil)
	resp := httptest.NewRecorder()
	s.ServeHTTP(resp, req)

	assert.Equal(t, 200, resp.Code)

	var response struct {
		Code int                    `json:"code"`
		Data []typedef.MultiStorage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &response))
	assert.Equal(t, 200, response.Code)
	assert.Len(t, response.Data, 1)
	assert.Equal(t, "local", response.Data[0].Name)
	assert.Equal(t, "file", response.Data[0].Type)
	assert.Equal(t, "/tmp/archives", response.Data[0].Path)
}

func TestCreateStorage(t *testing.T) {
	testDB, err := db.Initialize(":memory:")
	require.NoError(t, err)
	defer testDB.Close()

	cfg := &config.Config{
		Storage: []typedef.MultiStorage{
			{Storage: typedef.Storage{Name: "local", Type: "file", Path: "/tmp/archives"}},
		},
	}

	s := server.NewStorageTestServer(cfg, testDB)

	t.Run("file_storage", func(t *testing.T) {
		body, _ := json.Marshal(map[string]interface{}{
			"name": "local2",
			"type": "file",
			"path": "/tmp/archives2",
		})
		req, _ := http.NewRequest("POST", "/api/storage", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		resp := httptest.NewRecorder()
		s.ServeHTTP(resp, req)

		assert.Equal(t, 200, resp.Code)
		var response struct {
			Code int                  `json:"code"`
			Data typedef.MultiStorage `json:"data"`
		}
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &response))
		assert.Equal(t, 200, response.Code)
		assert.Equal(t, "local2", response.Data.Name)
		assert.Equal(t, "file", response.Data.Type)
		assert.Equal(t, "/tmp/archives2", response.Data.Path)

		// Verify via subsequent GET
		req, _ = http.NewRequest("GET", "/api/storage", nil)
		resp = httptest.NewRecorder()
		s.ServeHTTP(resp, req)
		var getResp struct {
			Code int                    `json:"code"`
			Data []typedef.MultiStorage `json:"data"`
		}
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &getResp))
		assert.Len(t, getResp.Data, 2)
	})

	t.Run("reject_s3_storage", func(t *testing.T) {
		body, _ := json.Marshal(map[string]interface{}{
			"name":            "s3-store",
			"type":            "s3",
			"endpoint":        "https://s3.example.com",
			"bucket":          "archives",
			"region":          "us-east-1",
			"accessKeyID":     "AKIAEXAMPLE",
			"secretAccessKey": "secretexample",
		})
		req, _ := http.NewRequest("POST", "/api/storage", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		resp := httptest.NewRecorder()
		s.ServeHTTP(resp, req)

		assert.Equal(t, 400, resp.Code)
		assert.Equal(t, 400, storageResponseCode(t, resp))
		req, _ = http.NewRequest("GET", "/api/storage", nil)
		resp = httptest.NewRecorder()
		s.ServeHTTP(resp, req)
		assert.NotContains(t, resp.Body.String(), "s3-store")
	})

	t.Run("duplicate_name", func(t *testing.T) {
		body, _ := json.Marshal(map[string]interface{}{
			"name": "local",
			"type": "file",
			"path": "/tmp/another",
		})
		req, _ := http.NewRequest("POST", "/api/storage", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		resp := httptest.NewRecorder()
		s.ServeHTTP(resp, req)

		assert.Equal(t, 409, resp.Code)
		assert.Equal(t, 409, storageResponseCode(t, resp))
	})

	t.Run("empty_name", func(t *testing.T) {
		body, _ := json.Marshal(map[string]interface{}{
			"type": "file",
			"path": "/tmp/no-name",
		})
		req, _ := http.NewRequest("POST", "/api/storage", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		resp := httptest.NewRecorder()
		s.ServeHTTP(resp, req)

		assert.Equal(t, 400, resp.Code)
		assert.Equal(t, 400, storageResponseCode(t, resp))
	})

	t.Run("invalid_type", func(t *testing.T) {
		body, _ := json.Marshal(map[string]interface{}{
			"name": "bad-type",
			"type": "ftp",
			"path": "/tmp/bad",
		})
		req, _ := http.NewRequest("POST", "/api/storage", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		resp := httptest.NewRecorder()
		s.ServeHTTP(resp, req)

		assert.Equal(t, 400, resp.Code)
		assert.Equal(t, 400, storageResponseCode(t, resp))
	})
}

func TestUpdateStorage(t *testing.T) {
	testDB, err := db.Initialize(":memory:")
	require.NoError(t, err)
	defer testDB.Close()

	cfg := &config.Config{
		Storage: []typedef.MultiStorage{
			{Storage: typedef.Storage{Name: "local", Type: "file", Path: "/tmp/archives"}},
		},
	}

	s := server.NewStorageTestServer(cfg, testDB)

	t.Run("reject_s3_update", func(t *testing.T) {
		for _, key := range []string{"type", "Type"} {
			body, _ := json.Marshal(map[string]string{key: "s3", "path": "/changed"})
			req, _ := http.NewRequest("PUT", "/api/storage/local", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			resp := httptest.NewRecorder()
			s.ServeHTTP(resp, req)
			assert.Equal(t, 400, resp.Code)
			assert.Equal(t, 400, storageResponseCode(t, resp))
			req, _ = http.NewRequest("GET", "/api/storage", nil)
			resp = httptest.NewRecorder()
			s.ServeHTTP(resp, req)
			var response struct {
				Data []typedef.MultiStorage `json:"data"`
			}
			require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &response))
			require.Len(t, response.Data, 1)
			assert.Equal(t, "file", response.Data[0].Type)
			assert.Equal(t, "/tmp/archives", response.Data[0].Path)
		}
	})

	t.Run("success", func(t *testing.T) {
		body, _ := json.Marshal(map[string]interface{}{
			"path": "/tmp/new-archives",
		})
		req, _ := http.NewRequest("PUT", "/api/storage/local", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		resp := httptest.NewRecorder()
		s.ServeHTTP(resp, req)

		assert.Equal(t, 200, resp.Code)
		var response struct {
			Code int                  `json:"code"`
			Data typedef.MultiStorage `json:"data"`
		}
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &response))
		assert.Equal(t, 200, response.Code)
		assert.Equal(t, "local", response.Data.Name) // name unchanged
		assert.Equal(t, "file", response.Data.Type)  // type unchanged
		assert.Equal(t, "/tmp/new-archives", response.Data.Path)
	})

	t.Run("not_found", func(t *testing.T) {
		body, _ := json.Marshal(map[string]interface{}{
			"path": "/tmp/whatever",
		})
		req, _ := http.NewRequest("PUT", "/api/storage/does-not-exist", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		resp := httptest.NewRecorder()
		s.ServeHTTP(resp, req)

		assert.Equal(t, 404, resp.Code)
		assert.Equal(t, 404, storageResponseCode(t, resp))
	})
}

func TestDeleteStorage(t *testing.T) {
	testDB, err := db.Initialize(":memory:")
	require.NoError(t, err)
	defer testDB.Close()

	cfg := &config.Config{
		Storage: []typedef.MultiStorage{
			{Storage: typedef.Storage{Name: "local", Type: "file", Path: "/tmp/archives"}},
			{Storage: typedef.Storage{Name: "local2", Type: "file", Path: "/tmp/archives2"}},
		},
	}

	s := server.NewStorageTestServer(cfg, testDB)

	t.Run("success", func(t *testing.T) {
		req, _ := http.NewRequest("DELETE", "/api/storage/local", nil)
		resp := httptest.NewRecorder()
		s.ServeHTTP(resp, req)

		assert.Equal(t, 200, resp.Code)
		var response struct {
			Code int `json:"code"`
			Data struct {
				Success bool `json:"success"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &response))
		assert.Equal(t, 200, response.Code)
		assert.True(t, response.Data.Success)

		// Verify removal via GET
		req, _ = http.NewRequest("GET", "/api/storage", nil)
		resp = httptest.NewRecorder()
		s.ServeHTTP(resp, req)
		var getResp struct {
			Code int                    `json:"code"`
			Data []typedef.MultiStorage `json:"data"`
		}
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &getResp))
		assert.Len(t, getResp.Data, 1)
		assert.Equal(t, "local2", getResp.Data[0].Name)
	})

	t.Run("not_found", func(t *testing.T) {
		req, _ := http.NewRequest("DELETE", "/api/storage/does-not-exist", nil)
		resp := httptest.NewRecorder()
		s.ServeHTTP(resp, req)

		assert.Equal(t, 404, resp.Code)
		assert.Equal(t, 404, storageResponseCode(t, resp))
	})
}
