package vm

import "testing"

func TestDockerBackendDisabledByDefault(t *testing.T) {
	t.Setenv("MINIBLUE_ENABLE_DOCKER_BACKENDS", "")
	t.Setenv("MINIBLUE_DISABLE_DOCKER", "")

	if h := NewHandler(nil); h.dockerAvail {
		t.Fatal("VM Docker backend should be disabled unless explicitly enabled")
	}
}
