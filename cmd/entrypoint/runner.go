//go:build !windows
// +build !windows

/*
Copyright 2021 The Tekton Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"syscall"
	"time"

	"github.com/puppetlabs/leg/timeutil/pkg/retry"
	"github.com/puppetlabs/relay-sdk-go/pkg/task"
	"github.com/puppetlabs/relay-sdk-go/pkg/taskutil"
	"github.com/tektoncd/pipeline/pkg/entrypoint"
	"github.com/tektoncd/pipeline/pkg/pod"
)

// TODO(jasonhall): Test that original exit code is propagated and that
// stdout/stderr are collected -- needs e2e tests.

const (
	TimerStepInit = "relay.step.init"
)

// realRunner actually runs commands.
type realRunner struct {
	signals chan os.Signal
}

var _ entrypoint.Runner = (*realRunner)(nil)

// Run executes the entrypoint.
func (rr *realRunner) Run(ctx context.Context, args ...string) error {
	if len(args) == 0 {
		return nil
	}
	name, args := args[0], args[1:]

	// TODO Ignore errors for now
	_ = timing()
	_ = setEnvironmentVariablesFromMetadata()
	_ = validateSchemas()

	// Receive system signals on "rr.signals"
	if rr.signals == nil {
		rr.signals = make(chan os.Signal, 1)
	}
	defer close(rr.signals)
	signal.Notify(rr.signals)
	defer signal.Reset()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// dedicated PID group used to forward signals to
	// main process and all children
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if os.Getenv("TEKTON_RESOURCE_NAME") == "" && os.Getenv(pod.TektonHermeticEnvVar) == "1" {
		dropNetworking(cmd)
	}

	// Start defined command
	if err := cmd.Start(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return context.DeadlineExceeded
		}
		return err
	}

	// Goroutine for signals forwarding
	go func() {
		for s := range rr.signals {
			// Forward signal to main process and all children
			if s != syscall.SIGCHLD {
				_ = syscall.Kill(-cmd.Process.Pid, s.(syscall.Signal))
			}
		}
	}()

	// Wait for command to exit
	if err := cmd.Wait(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return context.DeadlineExceeded
		}
		return err
	}

	return nil
}

func setEnvironmentVariablesFromMetadata() error {
	planOpts := taskutil.DefaultPlanOptions{}
	t := task.NewTaskInterface(planOpts)

	data, err := t.ReadEnvironmentVariables()
	if err != nil {
		return err
	}

	var ev map[string]interface{}
	err = json.Unmarshal(data, &ev)
	if err != nil {
		return err
	}
	for name, value := range ev {
		os.Setenv(name, fmt.Sprintf("%v", value))
	}

	return nil
}

func timing() error {
	mu, err := taskutil.MetadataURL(path.Join("/timers", url.PathEscape(TimerStepInit)))
	if err != nil {
		return err
	}

	te, err := url.Parse(mu)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPut, te.String(), nil)
	if err != nil {
		return err
	}

	_, err = getResponse(req, 5*time.Second, []retry.WaitOption{})
	if err != nil {
		return err
	}

	return nil
}

func validateSchemas() error {
	mu, err := taskutil.MetadataURL("validate")
	if err != nil {
		return err
	}

	te, err := url.Parse(mu)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, te.String(), nil)
	if err != nil {
		return err
	}

	// We are ignoring the response for now because this endpoint just sends
	// all validation errors to the error capturing system.
	_, err = getResponse(req, 5*time.Second, []retry.WaitOption{})
	if err != nil {
		return err
	}

	return nil
}

func getResponse(request *http.Request, timeout time.Duration, waitOptions []retry.WaitOption) (*http.Response, error) {
	ctx := context.Background()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var response *http.Response
	err := retry.Wait(ctx, func(ctx context.Context) (bool, error) {
		var rerr error
		response, rerr = http.DefaultClient.Do(request)
		if rerr != nil {
			return false, rerr
		}

		if response != nil {
			// TODO Consider expanding to all 5xx (and possibly some 4xx) status codes
			switch response.StatusCode {
			case http.StatusInternalServerError, http.StatusBadGateway,
				http.StatusServiceUnavailable, http.StatusGatewayTimeout:
				return false, nil
			}
		}

		return true, nil
	}, waitOptions...)
	if err != nil {
		return nil, err
	}

	return response, nil
}
