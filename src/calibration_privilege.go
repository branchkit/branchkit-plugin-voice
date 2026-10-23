package main

import (
	"errors"
	"sync/atomic"

	"github.com/branchkit/plugin-sdk-go"
)

// calibrationPrivilege is what the platform's trial.* operations require:
// they put other plugins' commands into their context and ask those plugins
// for sample phrases. It is optional in the manifest, so revoking it turns
// off calibration's contexts and nothing else; voice asks for it the first
// time a trial is refused.
const calibrationPrivilege = "calibration"

// calibrationPrivilegeReason is shown beside the Approve button.
const calibrationPrivilegeReason = "Calibration puts each command into its context (a mode, a page's hints) while you practise saying it."

// beginTrialOrAsk opens a calibration trial. When the platform refuses it
// because voice does not hold the calibration privilege, it files the
// request, once per run (the person approves it in Settings, and a denial
// is remembered there, so asking again would only repeat it; a call that
// failed outright is retried on the next refusal), and returns
// the refusal: the walk goes on without the command's context, as for any
// failed trial.
func beginTrialOrAsk(begin func() (string, error), ask func() (string, error), asked *atomic.Bool) (string, error) {
	id, err := begin()
	if err == nil || !errors.Is(err, branchkit.ErrForbidden) {
		return id, err
	}
	if asked.CompareAndSwap(false, true) {
		status, askErr := ask()
		if askErr != nil {
			// The call failed before anything was filed: ask again next time.
			asked.Store(false)
			branchkit.Logf("voice", "calibration: privileges.request(%s): %v", calibrationPrivilege, askErr)
		} else {
			branchkit.Logf("voice", "calibration: needs the %s privilege (%s): approve it in Settings to practise commands in their context", calibrationPrivilege, status)
		}
	}
	return "", err
}
