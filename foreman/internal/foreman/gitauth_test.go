package foreman

import "testing"

// TestGitAuthEnvKeepsTokenOutOfArgv pins: git auth env keeps token out of argv.
func TestGitAuthEnvKeepsTokenOutOfArgv(t *testing.T) {
	env := GitAuthEnv("tok")
	// base64("x-access-token:tok")
	if env["GIT_CONFIG_VALUE_0"] != "Authorization: Basic eC1hY2Nlc3MtdG9rZW46dG9r" ||
		env["GIT_CONFIG_KEY_0"] != "http.https://github.com/.extraheader" ||
		env["GIT_CONFIG_COUNT"] != "1" || env["GIT_TERMINAL_PROMPT"] != "0" {
		t.Errorf("got %v", env)
	}
}
