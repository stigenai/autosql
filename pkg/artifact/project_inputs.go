package artifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

const ProjectInputsMetadata = "autosql.project.inputs"
const projectBindingVersion = "autosql.project-attestation/v1"

type projectBinding struct {
	Version      string            `json:"version"`
	InputDigest  string            `json:"input_digest"`
	StageConfigs map[string]string `json:"stage_configs"`
}

// ProjectStageConfigDigest composes the existing stage context with the
// complete effective input identity without changing the artifact wire format.
func ProjectStageConfigDigest(stage, original, inputs string) (string, error) {
	if !projectStage(stage) || !digestPattern.MatchString(original) || !digestPattern.MatchString(inputs) {
		return "", fail("project_stage_context", ErrInvalid)
	}
	digest := sha256.Sum256([]byte(projectBindingVersion + "\x00" + stage + "\x00" + original + "\x00" + inputs))
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func projectStage(stage string) bool {
	return stage == "replay_simulation" || stage == "safety" || stage == "policy_precheck_guardrail"
}

// BindProjectInputs is a pre-signing operation for generated releases. Approval
// must already cover this same input digest through the guardrail bundle.
// Edited project releases are unsupported and refuse until separately attested.
func (a *Artifact) BindProjectInputs(inputs string) error {
	if err := a.canBindProjectInputs(inputs); err != nil {
		return err
	}
	binding := projectBinding{Version: projectBindingVersion, InputDigest: inputs, StageConfigs: map[string]string{}}
	attestations := make([]ValidationAttestation, len(a.ValidationAttestations))
	for index, att := range a.ValidationAttestations {
		composed, err := ProjectStageConfigDigest(att.Stage, att.ConfigDigest, inputs)
		if err != nil {
			return err
		}
		binding.StageConfigs[att.Stage] = att.ConfigDigest
		attestations[index] = withProjectConfig(att, composed)
	}
	raw, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	candidate := *a
	candidate.Metadata = clone(a.Metadata)
	candidate.Metadata[ProjectInputsMetadata] = string(raw)
	candidate.ValidationAttestations = attestations
	if err := candidate.validateUnsigned(); err != nil {
		return err
	}
	candidate.Digest, err = digest(candidate)
	if err != nil {
		return err
	}
	*a = candidate
	return nil
}

func (a Artifact) canBindProjectInputs(inputs string) error {
	if a.Signature.Value != "" || a.EditProvenance != nil || a.Origin.Kind != "generated" || !digestPattern.MatchString(inputs) {
		return fail("project_binding", ErrInvalid)
	}
	if _, exists := a.Metadata[ProjectInputsMetadata]; exists {
		return fail("project_binding_exists", ErrInvalid)
	}
	return a.validateUnsigned()
}

func withProjectConfig(att ValidationAttestation, digest string) ValidationAttestation {
	att.ConfigDigest = digest
	if att.Simulation != nil {
		copy := *att.Simulation
		copy.ConfigDigest = digest
		att.Simulation = &copy
	}
	if att.Safety != nil {
		copy := *att.Safety
		copy.ConfigDigest = digest
		att.Safety = &copy
	}
	if att.Policy != nil {
		copy := *att.Policy
		copy.ConfigDigest = digest
		att.Policy = &copy
	}
	if att.Precheck != nil {
		copy := *att.Precheck
		copy.ConfigDigest = digest
		att.Precheck = &copy
	}
	return att
}

// ProjectInputDigest reads and structurally validates the signed metadata
// claim. It is not a recomputation or verification of current project inputs.
func (a Artifact) ProjectInputDigest() (string, bool, error) {
	raw, exists := a.Metadata[ProjectInputsMetadata]
	if !exists {
		return "", false, nil
	}
	var binding projectBinding
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&binding)
	canonical, _ := json.Marshal(binding)
	if err != nil || string(canonical) != raw || binding.Version != projectBindingVersion || !digestPattern.MatchString(binding.InputDigest) {
		return "", true, fail("project_binding", ErrInvalid)
	}
	if err := a.validateProjectStages(binding); err != nil {
		return "", true, err
	}
	return binding.InputDigest, true, nil
}

func (a Artifact) validateProjectStages(binding projectBinding) error {
	if a.Origin.Kind != "generated" || a.EditProvenance != nil || len(binding.StageConfigs) != 3 || len(a.ValidationAttestations) != 3 {
		return fail("project_stages", ErrInvalid)
	}
	seen := map[string]bool{}
	for _, att := range a.ValidationAttestations {
		expected, err := ProjectStageConfigDigest(att.Stage, binding.StageConfigs[att.Stage], binding.InputDigest)
		if err != nil || seen[att.Stage] || att.ConfigDigest != expected {
			return fail("project_stage_context", ErrInvalid)
		}
		seen[att.Stage] = true
	}
	return nil
}

func (a Artifact) verifyProjectInputs(expected string) error {
	actual, exists, err := a.ProjectInputDigest()
	if err != nil {
		return err
	}
	if !exists && expected == "" {
		return nil
	}
	if !exists || !digestPattern.MatchString(expected) || actual != expected {
		return fail("project_inputs_current", ErrInvalid)
	}
	return nil
}
