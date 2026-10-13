package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseConfigDefaults(t *testing.T) {
	cfg, err := parseConfig([]byte(`
image: golang:1.27
timeout: 10m
steps:
  - name: test
    run: go test ./...
  - name: lint
    image: alpine:3.20
    timeout: 20m
    run: echo lint
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Steps[0].Image; got != "golang:1.27" {
		t.Errorf("step image = %q, want top-level image", got)
	}
	if got := cfg.Steps[0].Timeout; got != 10*time.Minute {
		t.Errorf("step timeout = %s, want build timeout", got)
	}
	if got := cfg.Steps[1].Timeout; got != 10*time.Minute {
		t.Errorf("step timeout = %s, want capped at build timeout", got)
	}
	if cfg.Deploy != nil {
		t.Fatal("deploy should be absent")
	}
}

func TestParseConfigDeploy(t *testing.T) {
	cfg, err := parseConfig([]byte(`
image: alpine:3.22
steps:
  - name: test
    run: echo ok
deploy:
  image: ghcr.io/acme/app
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Deploy.Image != "ghcr.io/acme/app" || cfg.Deploy.Dockerfile != "Dockerfile" || cfg.Deploy.Settle != 5*time.Second {
		t.Fatalf("deploy = %+v", cfg.Deploy)
	}
}

func TestParseConfigRejects(t *testing.T) {
	tests := map[string]struct {
		yaml string
		want string
	}{
		"unknown field": {
			yaml: "image: a\nsteps:\n  - name: x\n    command: echo\n",
			want: `unknown field "command"`,
		},
		"no steps":       {yaml: "image: a\n", want: "at least one step"},
		"missing run":    {yaml: "image: a\nsteps:\n  - name: x\n", want: "run is required"},
		"missing image":  {yaml: "steps:\n  - name: x\n    run: echo\n", want: "image is required"},
		"reserved name":  {yaml: "image: a\nsteps:\n  - name: checkout\n    run: echo\n", want: "reserved"},
		"duplicate name": {yaml: "image: a\nsteps:\n  - name: x\n    run: a\n  - name: x\n    run: b\n", want: "used twice"},
		"bad env":        {yaml: "image: a\nenv:\n  'A=B': c\nsteps:\n  - name: x\n    run: a\n", want: "not valid"},
		"long timeout":   {yaml: "image: a\ntimeout: 3h\nsteps:\n  - name: x\n    run: a\n", want: "timeout must be"},
		"tagged image":   {yaml: "image: a\nsteps:\n  - name: x\n    run: a\ndeploy:\n  image: app:latest\n", want: "deploy.image"},
		"bad dockerfile": {yaml: "image: a\nsteps:\n  - name: x\n    run: a\ndeploy:\n  image: app\n  dockerfile: ../Dockerfile\n", want: "deploy.dockerfile"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := parseConfig([]byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}
