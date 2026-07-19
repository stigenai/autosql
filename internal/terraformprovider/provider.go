package terraformprovider

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	core "autosql/pkg/integrations/terraform"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	providerschema "github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	resourceschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

const typeName = "autosql"

var (
	_ provider.Provider                = (*Provider)(nil)
	_ resource.Resource                = (*schemaResource)(nil)
	_ resource.ResourceWithImportState = (*schemaResource)(nil)
)

type Provider struct{ version string }

func New(version string) func() provider.Provider {
	return func() provider.Provider { return &Provider{version: version} }
}

type providerModel struct {
	BinaryPath     types.String `tfsdk:"binary_path"`
	ApplyConfigRef types.String `tfsdk:"apply_config_ref"`
}

type client struct {
	binaryPath      string
	applyConfigPath string
}

func (p *Provider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName, resp.Version = typeName, p.version
}

func (p *Provider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = providerschema.Schema{MarkdownDescription: "Manages digest-bound PostgreSQL schema and migration artifacts with the AutoSQL CLI.", Attributes: map[string]providerschema.Attribute{
		"binary_path":      providerschema.StringAttribute{Optional: true, Description: "Path to the released autosql binary. Defaults to autosql on PATH."},
		"apply_config_ref": providerschema.StringAttribute{Required: true, Sensitive: true, Description: "Opaque file:// reference to the AutoSQL production apply configuration. Contents never enter Terraform state."},
	}}
}

func (p *Provider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	binary := "autosql"
	if !cfg.BinaryPath.IsNull() && !cfg.BinaryPath.IsUnknown() && strings.TrimSpace(cfg.BinaryPath.ValueString()) != "" {
		binary = cfg.BinaryPath.ValueString()
	}
	ref := cfg.ApplyConfigRef.ValueString()
	configPath, err := localPath(ref)
	if err != nil {
		resp.Diagnostics.AddError("Invalid apply configuration reference", "apply_config_ref must be an absolute file:// reference")
		return
	}
	if _, err := os.Stat(configPath); err != nil {
		resp.Diagnostics.AddError("Apply configuration unavailable", "The referenced AutoSQL apply configuration cannot be read.")
		return
	}
	c := &client{binaryPath: binary, applyConfigPath: configPath}
	resp.ResourceData, resp.DataSourceData = c, c
}

func (p *Provider) Resources(context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		func() resource.Resource { return &schemaResource{workflow: core.Declarative} },
		func() resource.Resource { return &schemaResource{workflow: core.Versioned} },
	}
}
func (p *Provider) DataSources(context.Context) []func() datasource.DataSource { return nil }

type resourceModel struct {
	ID                    types.String `tfsdk:"id"`
	SourceRef             types.String `tfsdk:"source_ref"`
	ArtifactDigest        types.String `tfsdk:"artifact_digest"`
	PolicyDigest          types.String `tfsdk:"policy_digest"`
	TargetSnapshot        types.String `tfsdk:"target_snapshot"`
	TargetID              types.String `tfsdk:"target_id"`
	Environment           types.String `tfsdk:"environment"`
	ConnectionRef         types.String `tfsdk:"connection_ref"`
	ApprovalRef           types.String `tfsdk:"approval_ref"`
	ApprovalDigest        types.String `tfsdk:"approval_digest"`
	DestroySourceRef      types.String `tfsdk:"destroy_source_ref"`
	DestroyArtifactDigest types.String `tfsdk:"destroy_artifact_digest"`
	DestroyApprovalRef    types.String `tfsdk:"destroy_approval_ref"`
	DestroyApprovalDigest types.String `tfsdk:"destroy_approval_digest"`
	ObservedDigest        types.String `tfsdk:"observed_digest"`
	AppliedAt             types.String `tfsdk:"applied_at"`
}

type schemaResource struct {
	workflow core.Workflow
	client   *client
}

func (r *schemaResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	suffix := "schema"
	if r.workflow == core.Versioned {
		suffix = "migration"
	}
	resp.TypeName = req.ProviderTypeName + "_" + suffix
}

func (r *schemaResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	c, ok := req.ProviderData.(*client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", "AutoSQL provider configuration has an invalid internal type.")
		return
	}
	r.client = c
}

func (r *schemaResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	required := func(description string) resourceschema.Attribute {
		return resourceschema.StringAttribute{Required: true, Description: description}
	}
	optional := func(description string) resourceschema.Attribute {
		return resourceschema.StringAttribute{Optional: true, Description: description}
	}
	resp.Schema = resourceschema.Schema{MarkdownDescription: "Applies an immutable, approved AutoSQL artifact. All credential-bearing values remain opaque references.", Attributes: map[string]resourceschema.Attribute{
		"id":                      required("Stable deployment identifier."),
		"source_ref":              required("Absolute file:// reference to the signed AutoSQL artifact."),
		"artifact_digest":         required("Expected sha256 digest of the artifact bytes."),
		"policy_digest":           required("Policy digest bound by the reviewed artifact."),
		"target_snapshot":         required("Target snapshot digest bound by the reviewed artifact."),
		"target_id":               required("Non-secret target identity."),
		"environment":             required("Deployment environment."),
		"connection_ref":          resourceschema.StringAttribute{Required: true, Sensitive: true, Description: "Opaque env:// or file:// database connection reference. The provider never resolves it."},
		"approval_ref":            resourceschema.StringAttribute{Required: true, Sensitive: true, Description: "Absolute file:// reference to approval evidence."},
		"approval_digest":         required("Expected sha256 digest of approval evidence."),
		"destroy_source_ref":      optional("Optional signed rollback artifact used during terraform destroy."),
		"destroy_artifact_digest": optional("Expected digest for destroy_source_ref."),
		"destroy_approval_ref":    resourceschema.StringAttribute{Optional: true, Sensitive: true, Description: "Approval evidence for the destructive artifact."},
		"destroy_approval_digest": optional("Expected digest for destroy approval evidence."),
		"observed_digest":         resourceschema.StringAttribute{Computed: true, Description: "Artifact digest most recently verified by the provider."},
		"applied_at":              resourceschema.StringAttribute{Computed: true, Description: "UTC time of the most recent successful apply."},
	}}
}

func (r *schemaResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan resourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.apply(ctx, &plan, false); err != nil {
		resp.Diagnostics.AddError("AutoSQL apply failed", safeError(err))
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *schemaResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan resourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.apply(ctx, &plan, false); err != nil {
		resp.Diagnostics.AddError("AutoSQL update failed", safeError(err))
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *schemaResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state resourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if _, err := verifyPair(state.SourceRef.ValueString(), state.ArtifactDigest.ValueString()); err != nil {
		resp.Diagnostics.AddWarning("AutoSQL artifact unavailable or changed", "The local artifact no longer matches Terraform state; the database was not contacted and credentials were not resolved.")
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *schemaResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state resourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	configured := []string{state.DestroySourceRef.ValueString(), state.DestroyArtifactDigest.ValueString(), state.DestroyApprovalRef.ValueString(), state.DestroyApprovalDigest.ValueString()}
	count := 0
	for _, value := range configured {
		if strings.TrimSpace(value) != "" {
			count++
		}
	}
	if count != 0 && count != len(configured) {
		resp.Diagnostics.AddError("Incomplete destructive approval", "All destroy_source_ref, destroy_artifact_digest, destroy_approval_ref, and destroy_approval_digest values are required together.")
		return
	}
	if count == len(configured) {
		if err := r.apply(ctx, &state, true); err != nil {
			resp.Diagnostics.AddError("AutoSQL destroy failed", safeError(err))
			return
		}
	}
	resp.State.RemoveResource(ctx)
}

func (r *schemaResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	var imported struct {
		ID, SourceRef, ArtifactDigest, PolicyDigest, TargetSnapshot, TargetID, Environment, ConnectionRef, ApprovalRef, ApprovalDigest string
		DestroySourceRef, DestroyArtifactDigest, DestroyApprovalRef, DestroyApprovalDigest                                             string
	}
	if err := json.Unmarshal([]byte(req.ID), &imported); err != nil {
		resp.Diagnostics.AddError("Invalid import identity", "Import requires a JSON object containing the non-secret resource attributes and opaque references.")
		return
	}
	model := resourceModel{ID: types.StringValue(imported.ID), SourceRef: types.StringValue(imported.SourceRef), ArtifactDigest: types.StringValue(imported.ArtifactDigest), PolicyDigest: types.StringValue(imported.PolicyDigest), TargetSnapshot: types.StringValue(imported.TargetSnapshot), TargetID: types.StringValue(imported.TargetID), Environment: types.StringValue(imported.Environment), ConnectionRef: types.StringValue(imported.ConnectionRef), ApprovalRef: types.StringValue(imported.ApprovalRef), ApprovalDigest: types.StringValue(imported.ApprovalDigest), ObservedDigest: types.StringValue(imported.ArtifactDigest)}
	optional := func(value string) types.String {
		if value == "" {
			return types.StringNull()
		}
		return types.StringValue(value)
	}
	model.DestroySourceRef = optional(imported.DestroySourceRef)
	model.DestroyArtifactDigest = optional(imported.DestroyArtifactDigest)
	model.DestroyApprovalRef = optional(imported.DestroyApprovalRef)
	model.DestroyApprovalDigest = optional(imported.DestroyApprovalDigest)
	if err := (core.ResourceConfig{ID: imported.ID, Workflow: r.workflow, SourceRef: imported.SourceRef, ArtifactDigest: imported.ArtifactDigest, PolicyDigest: imported.PolicyDigest, TargetSnapshot: imported.TargetSnapshot, TargetID: imported.TargetID, Environment: imported.Environment, ConnectionRef: imported.ConnectionRef}).Validate(); err != nil {
		resp.Diagnostics.AddError("Invalid import identity", "Import attributes violate the AutoSQL non-secret state contract.")
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}

func (r *schemaResource) apply(ctx context.Context, m *resourceModel, destroy bool) error {
	if r.client == nil {
		return errors.New("provider is not configured")
	}
	sourceRef, artifactDigest, approvalRef, approvalDigest := m.SourceRef.ValueString(), m.ArtifactDigest.ValueString(), m.ApprovalRef.ValueString(), m.ApprovalDigest.ValueString()
	if destroy {
		sourceRef, artifactDigest, approvalRef, approvalDigest = m.DestroySourceRef.ValueString(), m.DestroyArtifactDigest.ValueString(), m.DestroyApprovalRef.ValueString(), m.DestroyApprovalDigest.ValueString()
	}
	cfg := core.ResourceConfig{ID: m.ID.ValueString(), Workflow: r.workflow, SourceRef: sourceRef, ArtifactDigest: artifactDigest, PolicyDigest: m.PolicyDigest.ValueString(), TargetSnapshot: m.TargetSnapshot.ValueString(), TargetID: m.TargetID.ValueString(), Environment: m.Environment.ValueString(), ConnectionRef: m.ConnectionRef.ValueString(), Destroy: destroy}
	if err := cfg.Validate(); err != nil {
		return err
	}
	artifactPath, err := verifyPair(sourceRef, artifactDigest)
	if err != nil {
		return err
	}
	if _, err := verifyPair(approvalRef, approvalDigest); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, r.client.binaryPath, "apply", "--artifact", artifactPath, "--no-edits", "--json")
	cmd.Env = append(os.Environ(), "AUTOSQL_APPLY_CONFIG="+r.client.applyConfigPath)
	var stderr strings.Builder
	cmd.Stdout, cmd.Stderr = nil, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("autosql exited unsuccessfully: %s", redact(stderr.String()))
	}
	m.ObservedDigest = types.StringValue(artifactDigest)
	m.AppliedAt = types.StringValue(time.Now().UTC().Format(time.RFC3339Nano))
	return nil
}

func verifyPair(ref, expected string) (string, error) {
	path, err := localPath(ref)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("referenced material is unavailable")
	}
	h := sha256.Sum256(b)
	actual := "sha256:" + hex.EncodeToString(h[:])
	if len(expected) != len(actual) || subtle.ConstantTimeCompare([]byte(expected), []byte(actual)) != 1 {
		return "", errors.New("referenced material digest mismatch")
	}
	return path, nil
}

func localPath(ref string) (string, error) {
	if !strings.HasPrefix(ref, "file://") {
		return "", errors.New("reference must use file://")
	}
	p := strings.TrimPrefix(ref, "file://")
	if !filepath.IsAbs(p) {
		return "", errors.New("reference path must be absolute")
	}
	return p, nil
}

func safeError(err error) string { return redact(err.Error()) }
func redact(value string) string {
	for _, marker := range []string{"postgres://", "postgresql://"} {
		if i := strings.Index(value, marker); i >= 0 {
			return value[:i] + "[REDACTED]"
		}
	}
	return strings.TrimSpace(value)
}

// Force the framework to retain the custom JSON import path in generated docs.
var _ = path.Root
