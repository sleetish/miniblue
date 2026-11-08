package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestStructuredLoggerOmitsQueryParameters(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = writer
	t.Cleanup(func() { os.Stdout = old })
	handler := StructuredLogger(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/retrieve?code=secret-value", nil))
	writer.Close()
	output, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(output), "secret-value") || !strings.Contains(string(output), "/api/retrieve") {
		t.Fatalf("unexpected request log: %s", output)
	}
}
