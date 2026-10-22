// Package health reports on disks and the database for the admin dashboard
// and alerts.
package health

import (
	"context"
	"encoding/json"
	"os/exec"
	"time"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/volume"
)

type Report struct {
	OK       bool            `json:"ok"`
	ReadOnly bool            `json:"read_only"`
	Volumes  []volume.Status `json:"volumes"`
	State    volume.Status   `json:"state"`
	Smart    []SmartStatus   `json:"smart,omitempty"`
	Jobs     map[string]int  `json:"jobs,omitempty"`
	Warnings []string        `json:"warnings,omitempty"`
	At       time.Time       `json:"at"`
}

type SmartStatus struct {
	Device  string `json:"device"`
	Passed  *bool  `json:"passed,omitempty"`
	Model   string `json:"model,omitempty"`
	TempC   int    `json:"temperature_c,omitempty"`
	Problem string `json:"problem,omitempty"`
}

// Smart queries smartctl's JSON output for a device. USB enclosures often
// don't pass SMART through; that is reported as a problem, not a failure.
func Smart(ctx context.Context, device string) SmartStatus {
	s := SmartStatus{Device: device}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "smartctl", "-H", "-i", "-A", "-j", device).Output()
	if len(out) == 0 {
		s.Problem = "smartctl unavailable or needs root: " + errString(err)
		return s
	}
	var v struct {
		ModelName   string `json:"model_name"`
		SmartStatus *struct {
			Passed bool `json:"passed"`
		} `json:"smart_status"`
		Temperature struct {
			Current int `json:"current"`
		} `json:"temperature"`
		Smartctl struct {
			Messages []struct {
				String string `json:"string"`
			} `json:"messages"`
		} `json:"smartctl"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		s.Problem = "could not parse smartctl output"
		return s
	}
	s.Model, s.TempC = v.ModelName, v.Temperature.Current
	if v.SmartStatus != nil {
		s.Passed = &v.SmartStatus.Passed
	} else if len(v.Smartctl.Messages) > 0 {
		s.Problem = v.Smartctl.Messages[0].String
	}
	return s
}

func errString(err error) string {
	if err == nil {
		return "no output"
	}
	return err.Error()
}
