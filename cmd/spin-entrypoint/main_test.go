package main

import (
	"encoding/base64"
	"strings"
	"testing"

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

func TestBuildPlanBasic(t *testing.T) {
	p, err := buildPlan([]string{"up", "--from=ghcr.io/x/app:v1", "--listen=0.0.0.0:3000"},
		[]string{"HOME=/root", "PATH=/bin:/sbin", "SPIN_VARIABLE_GREETING=hi"})
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
	n := 0
	for _, kv := range p.env {
		if strings.HasPrefix(kv, "HOME=") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("HOME set %d times", n)
	}
	if p.runtimeConfig != nil {
		t.Fatal("unexpected runtime config")
	}
}

func TestBuildPlanRuntimeConfig(t *testing.T) {
	doc := "[key_value_store.default]\ntype = \"spin\"\n"
	env := []string{runtimecontract.RuntimeConfigEnv + "=" + base64.StdEncoding.EncodeToString([]byte(doc))}
	p, err := buildPlan([]string{"up", "--runtime-config-file=" + runtimecontract.RuntimeConfigPath}, env)
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

func TestBuildPlanRejects(t *testing.T) {
	cfg := runtimecontract.RuntimeConfigEnv + "=" + base64.StdEncoding.EncodeToString([]byte("x"))
	flag := "--runtime-config-file=" + runtimecontract.RuntimeConfigPath
	cases := map[string]struct {
		args []string
		env  []string
		want string
	}{
		"no args":         {nil, nil, "usage"},
		"other command":   {[]string{"build"}, nil, "unsupported command"},
		"shell":           {[]string{"/bin/sh", "-c", "id"}, nil, "unsupported command"},
		"config no flag":  {[]string{"up"}, []string{cfg}, "missing from the arguments"},
		"flag no config":  {[]string{"up", flag}, nil, "is not set"},
		"other path":      {[]string{"up", "--runtime-config-file=/etc/passwd"}, nil, "must be passed as"},
		"separate value":  {[]string{"up", "--runtime-config-file", "/x"}, nil, "must be passed as"},
		"bad base64":      {[]string{"up", flag}, []string{runtimecontract.RuntimeConfigEnv + "=***"}, "not valid base64"},
		"oversize config": {[]string{"up", flag}, []string{runtimecontract.RuntimeConfigEnv + "=" + strings.Repeat("A", 100000)}, "exceeds"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := buildPlan(tc.args, tc.env)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestErrorsDoNotEchoConfig(t *testing.T) {
	const marker = "SECRET-LOOKING-VALUE"
	_, err := buildPlan([]string{"up"}, []string{runtimecontract.RuntimeConfigEnv + "=" + marker})
	if err == nil || strings.Contains(err.Error(), marker) {
		t.Fatalf("error echoes the runtime config: %v", err)
	}
}
