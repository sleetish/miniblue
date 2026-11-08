package aci

import (
	"testing"

	"github.com/moabukar/miniblue/internal/store"
)

func TestDockerBackendDisabledByDefault(t *testing.T) {
	t.Setenv("MINIBLUE_ENABLE_DOCKER_BACKENDS", "")

	if h := NewHandler(store.New()); h.dockerAvail {
		t.Fatal("ACI Docker backend should be disabled unless explicitly enabled")
	}
}
