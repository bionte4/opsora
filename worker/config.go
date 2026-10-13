package main

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	configFile          = ".opsora.yml"
	checkoutStepName    = "checkout"
	defaultBuildTimeout = 30 * time.Minute
	maxBuildTimeout     = 2 * time.Hour
	maxSteps            = 50
	defaultSettle       = 5 * time.Second
	maxSettle           = 2 * time.Minute
)

var unknownField = regexp.MustCompile(`field (\S+) not found in type \S+`)

type pipelineConfig struct {
	Image   string            `yaml:"image"`
	Timeout time.Duration     `yaml:"timeout"`
	Env     map[string]string `yaml:"env"`
	Steps   []stepConfig      `yaml:"steps"`
	Deploy  *deployConfig     `yaml:"deploy"`
}

type deployConfig struct {
	Image      string        `yaml:"image"`
	Dockerfile string        `yaml:"dockerfile"`
	Settle     time.Duration `yaml:"settle"`
}

type stepConfig struct {
	Name    string            `yaml:"name"`
	Image   string            `yaml:"image"`
	Run     string            `yaml:"run"`
	Timeout time.Duration     `yaml:"timeout"`
	Env     map[string]string `yaml:"env"`
}

// parseConfig validates the pipeline file and fills in defaults, so callers
// can rely on every step having an image and a timeout.
func parseConfig(data []byte) (pipelineConfig, error) {
	var cfg pipelineConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		var typeErr *yaml.TypeError
		if errors.As(err, &typeErr) {
			msgs := make([]string, len(typeErr.Errors))
			for i, m := range typeErr.Errors {
				msgs[i] = unknownField.ReplaceAllString(m, `unknown field "$1"`)
			}
			return pipelineConfig{}, fmt.Errorf("%s: %s", configFile, strings.Join(msgs, "; "))
		}
		return pipelineConfig{}, fmt.Errorf("parse %s: %w", configFile, err)
	}

	if cfg.Timeout == 0 {
		cfg.Timeout = defaultBuildTimeout
	}
	if cfg.Timeout < 0 || cfg.Timeout > maxBuildTimeout {
		return pipelineConfig{}, fmt.Errorf("timeout must be between 1s and %s", maxBuildTimeout)
	}
	if err := validateEnv(cfg.Env); err != nil {
		return pipelineConfig{}, err
	}
	if len(cfg.Steps) == 0 {
		return pipelineConfig{}, errors.New("steps must list at least one step")
	}
	if len(cfg.Steps) > maxSteps {
		return pipelineConfig{}, fmt.Errorf("steps must list at most %d steps", maxSteps)
	}

	seen := map[string]bool{checkoutStepName: true}
	for i := range cfg.Steps {
		step := &cfg.Steps[i]
		step.Name = strings.TrimSpace(step.Name)
		switch {
		case step.Name == "":
			return pipelineConfig{}, fmt.Errorf("step %d: name is required", i+1)
		case len(step.Name) > 100:
			return pipelineConfig{}, fmt.Errorf("step %q: name is longer than 100 characters", step.Name)
		case seen[step.Name]:
			return pipelineConfig{}, fmt.Errorf("step %q: name is used twice or reserved", step.Name)
		case strings.TrimSpace(step.Run) == "":
			return pipelineConfig{}, fmt.Errorf("step %q: run is required", step.Name)
		}
		seen[step.Name] = true

		if step.Image == "" {
			step.Image = cfg.Image
		}
		if step.Image == "" {
			return pipelineConfig{}, fmt.Errorf("step %q: image is required when no top-level image is set", step.Name)
		}
		if step.Timeout < 0 {
			return pipelineConfig{}, fmt.Errorf("step %q: timeout must be positive", step.Name)
		}
		if step.Timeout == 0 || step.Timeout > cfg.Timeout {
			step.Timeout = cfg.Timeout
		}
		if err := validateEnv(step.Env); err != nil {
			return pipelineConfig{}, fmt.Errorf("step %q: %w", step.Name, err)
		}
	}
	if err := validateDeploy(cfg.Deploy); err != nil {
		return pipelineConfig{}, err
	}
	return cfg, nil
}

var imageName = regexp.MustCompile(`^[a-z0-9]+([._/-][a-z0-9]+)*$`)

func validateDeploy(d *deployConfig) error {
	if d == nil {
		return nil
	}
	if !imageName.MatchString(d.Image) {
		return fmt.Errorf("deploy.image %q must be a lowercase name without a tag, like myapp or ghcr.io/org/myapp", d.Image)
	}
	if d.Dockerfile == "" {
		d.Dockerfile = "Dockerfile"
	}
	if strings.Contains(d.Dockerfile, "..") || strings.HasPrefix(d.Dockerfile, "/") || strings.TrimSpace(d.Dockerfile) != d.Dockerfile {
		return fmt.Errorf("deploy.dockerfile %q must be a relative path inside the repository", d.Dockerfile)
	}
	if d.Settle == 0 {
		d.Settle = defaultSettle
	}
	if d.Settle < time.Second || d.Settle > maxSettle {
		return fmt.Errorf("deploy.settle must be between 1s and %s", maxSettle)
	}
	return nil
}

func validateEnv(env map[string]string) error {
	for key := range env {
		if key == "" || strings.ContainsAny(key, "= \t\n") {
			return fmt.Errorf("env name %q is not valid", key)
		}
	}
	return nil
}
