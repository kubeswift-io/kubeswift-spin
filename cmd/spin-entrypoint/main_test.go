package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"github.com/kubeswift-io/kubeswift-spin/internal/runtimecontract"
)

func envValue(env []string, key string) (string, bool) {
	for _, kv := range env {
		if k, v, _ := strings.Cut(kv, "="); k == key {
			return v, true
		}
	}
	return "", false
}

// files is an in-memory readFileFunc.
type files map[string]string

func (f files) read(path string) ([]byte, error) {
	if v, ok := f[path]; ok {
		return []byte(v), nil
	}
	return nil, os.ErrNotExist
}

func noFiles(string) ([]byte, error) { return nil, errors.New("no file access expected") }

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

const configFlag = "--runtime-config-file=" + runtimecontract.RuntimeConfigPath

func TestBuildPlanBasic(t *testing.T) {
	p, err := buildPlan([]string{"up", "--from=ghcr.io/x/app:v1", "--listen=0.0.0.0:3000"},
		[]string{"HOME=/root", "PATH=/bin:/sbin", "SPIN_VARIABLE_GREETING=hi", "DOCKER_CONFIG=/elsewhere"}, noFiles)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(p.argv, " ") != "spin up --from=ghcr.io/x/app:v1 --listen=0.0.0.0:3000" {
		t.Fatalf("argv %v", p.argv)
	}
	if v, _ := envValue(p.env, "HOME"); v != runtimecontract.HomeDir {
		t.Fatalf("HOME = %q", v)
	}
	if v, _ := envValue(p.env, "PATH"); v != "/usr/local/bin" {
		t.Fatalf("PATH = %q", v)
	}
	if v, _ := envValue(p.env, "SPIN_VARIABLE_GREETING"); v != "hi" {
		t.Fatal("application variable dropped")
	}
	if _, ok := envValue(p.env, "DOCKER_CONFIG"); ok {
		t.Fatal("DOCKER_CONFIG passed through")
	}
	if p.runtimeConfig != nil || p.dockerConfig != nil {
		t.Fatal("unexpected files")
	}
}

func TestBuildPlanInlineRuntimeConfig(t *testing.T) {
	doc := "[key_value_store.default]\ntype = \"spin\"\n"
	p, err := buildPlan([]string{"up", configFlag}, []string{runtimecontract.RuntimeConfigEnv + "=" + b64(doc)}, noFiles)
	if err != nil {
		t.Fatal(err)
	}
	if string(p.runtimeConfig) != doc {
		t.Fatalf("runtime config %q", p.runtimeConfig)
	}
	if _, ok := envValue(p.env, runtimecontract.RuntimeConfigEnv); ok {
		t.Fatal("runtime config variable leaked into the Spin environment")
	}
}

func TestBuildPlanSubstitutesSecrets(t *testing.T) {
	name := runtimecontract.SecretValueEnvPrefix + "0"
	doc := "[llm_compute]\ntype = \"remote_http\"\nurl = \"http://llm:8000\"\nauth_token = \"" +
		runtimecontract.SecretPlaceholderPrefix + name + "\"\n"
	secret := `s3cr"et\with "quotes` + "\n"
	p, err := buildPlan([]string{"up", configFlag},
		[]string{runtimecontract.RuntimeConfigEnv + "=" + b64(doc), name + "=" + secret}, noFiles)
	if err != nil {
		t.Fatal(err)
	}
	var tree map[string]map[string]string
	if err := toml.Unmarshal(p.runtimeConfig, &tree); err != nil {
		t.Fatalf("substituted config is not valid TOML: %v\n%s", err, p.runtimeConfig)
	}
	if tree["llm_compute"]["auth_token"] != secret || tree["llm_compute"]["url"] != "http://llm:8000" {
		t.Fatalf("substitution wrong: %v", tree)
	}
	if _, ok := envValue(p.env, name); ok {
		t.Fatal("secret variable leaked into the Spin environment")
	}
}

func TestBuildPlanMissingSecretDoesNotEcho(t *testing.T) {
	doc := "[x]\nkey = \"" + runtimecontract.SecretPlaceholderPrefix + runtimecontract.SecretValueEnvPrefix + "7\"\n"
	_, err := buildPlan([]string{"up", configFlag}, []string{runtimecontract.RuntimeConfigEnv + "=" + b64(doc)}, noFiles)
	if err == nil || !strings.Contains(err.Error(), "KUBESWIFT_SPIN_SECRET_7") {
		t.Fatalf("got %v", err)
	}
	doc = "[x]\nkey = \"" + runtimecontract.SecretPlaceholderPrefix + "HOME\"\n"
	if _, err := buildPlan([]string{"up", configFlag}, []string{runtimecontract.RuntimeConfigEnv + "=" + b64(doc), "HOME=/root"}, noFiles); err == nil {
		t.Fatal("placeholder naming a non-secret variable accepted")
	}
}

func TestBuildPlanRuntimeConfigFile(t *testing.T) {
	fs := files{runtimecontract.SecretRuntimeConfigPath: "[key_value_store.default]\ntype = \"redis\"\nurl = \"redis://:pw@r:6379\"\n"}
	p, err := buildPlan([]string{"up", configFlag},
		[]string{runtimecontract.RuntimeConfigFileEnv + "=" + runtimecontract.SecretRuntimeConfigPath}, fs.read)
	if err != nil {
		t.Fatal(err)
	}
	if string(p.runtimeConfig) != fs[runtimecontract.SecretRuntimeConfigPath] {
		t.Fatal("file content not used verbatim")
	}
	if _, err := buildPlan([]string{"up", configFlag}, []string{runtimecontract.RuntimeConfigFileEnv + "=/etc/shadow"}, fs.read); err == nil {
		t.Fatal("arbitrary runtime config path accepted")
	}
}

func TestBuildPlanRegistryAuth(t *testing.T) {
	a := runtimecontract.RegistryAuthDir + "/0.json"
	b := runtimecontract.RegistryAuthDir + "/1.json"
	fs := files{
		a: `{"auths":{"ghcr.io":{"auth":"Zmlyc3Q="},"r.example":{"auth":"b2xk"}}}`,
		b: `{"auths":{"r.example":{"auth":"bmV3"}}}`,
	}
	p, err := buildPlan([]string{"up"}, []string{runtimecontract.RegistryAuthFilesEnv + "=" + a + "," + b}, fs.read)
	if err != nil {
		t.Fatal(err)
	}
	var merged struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(p.dockerConfig, &merged); err != nil {
		t.Fatal(err)
	}
	if merged.Auths["ghcr.io"].Auth != "Zmlyc3Q=" || merged.Auths["r.example"].Auth != "bmV3" {
		t.Fatalf("merge wrong: %s", p.dockerConfig)
	}
	if _, ok := envValue(p.env, runtimecontract.RegistryAuthFilesEnv); ok {
		t.Fatal("registry auth variable leaked into the Spin environment")
	}
	for _, bad := range []string{"/etc/passwd", runtimecontract.RegistryAuthDir + "/../x.json", runtimecontract.RegistryAuthDir + "/0.txt"} {
		if _, err := buildPlan([]string{"up"}, []string{runtimecontract.RegistryAuthFilesEnv + "=" + bad}, fs.read); err == nil {
			t.Fatalf("path %q accepted", bad)
		}
	}
	fs[a] = `{"username":"x"}`
	if _, err := buildPlan([]string{"up"}, []string{runtimecontract.RegistryAuthFilesEnv + "=" + a}, fs.read); err == nil ||
		strings.Contains(err.Error(), "username") {
		t.Fatalf("non-docker-config file: %v", err)
	}
}

func TestBuildPlanRejects(t *testing.T) {
	cfg := runtimecontract.RuntimeConfigEnv + "=" + b64("x")
	cfgFile := runtimecontract.RuntimeConfigFileEnv + "=" + runtimecontract.SecretRuntimeConfigPath
	cases := map[string]struct {
		args []string
		env  []string
		want string
	}{
		"no args":         {nil, nil, "usage"},
		"other command":   {[]string{"build"}, nil, "unsupported command"},
		"shell":           {[]string{"/bin/sh", "-c", "id"}, nil, "unsupported command"},
		"config no flag":  {[]string{"up"}, []string{cfg}, "missing from the arguments"},
		"flag no config":  {[]string{"up", configFlag}, nil, "neither"},
		"both configs":    {[]string{"up", configFlag}, []string{cfg, cfgFile}, "mutually exclusive"},
		"other path":      {[]string{"up", "--runtime-config-file=/etc/passwd"}, nil, "must be passed as"},
		"separate value":  {[]string{"up", "--runtime-config-file", "/x"}, nil, "must be passed as"},
		"bad base64":      {[]string{"up", configFlag}, []string{runtimecontract.RuntimeConfigEnv + "=***"}, "not valid base64"},
		"oversize config": {[]string{"up", configFlag}, []string{runtimecontract.RuntimeConfigEnv + "=" + strings.Repeat("A", 100000)}, "exceeds"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := buildPlan(tc.args, tc.env, noFiles)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestErrorsDoNotEchoConfig(t *testing.T) {
	const marker = "SECRET-LOOKING-VALUE"
	_, err := buildPlan([]string{"up"}, []string{runtimecontract.RuntimeConfigEnv + "=" + marker}, noFiles)
	if err == nil || strings.Contains(err.Error(), marker) {
		t.Fatalf("error echoes the runtime config: %v", err)
	}
	doc := "not toml " + marker + " " + runtimecontract.SecretPlaceholderPrefix
	_, err = buildPlan([]string{"up", configFlag}, []string{runtimecontract.RuntimeConfigEnv + "=" + b64(doc)}, noFiles)
	if err == nil || strings.Contains(err.Error(), marker) {
		t.Fatalf("parse error echoes the runtime config: %v", err)
	}
}
