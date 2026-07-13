package start

import (
	"context"
	"fmt"

	"autosql/pkg/zdm/backfill"
	"autosql/pkg/zdm/shadowsync"
	"autosql/pkg/zdm/virtualschema"
)

// Pipeline binds the coordinator to the concrete expand/compatibility/backfill
// implementations. Expand executes a previously verified immutable expand plan.
type Pipeline struct {
	Spec           Spec
	Expand         func(context.Context) error
	VirtualConfig  virtualschema.Config
	Virtual        virtualschema.Spec
	ShadowConfig   shadowsync.Config
	Shadow         shadowsync.Spec
	ShadowPolicy   shadowsync.Policy
	BackfillConfig backfill.Config
	Backfills      []backfill.Spec
}

// RunPipeline is the preferred entry point. It refuses cross-target or
// cross-database bindings before durable intent is recorded.
func RunPipeline(ctx context.Context, cfg Config, p Pipeline) (Status, error) {
	if p.VirtualConfig.URL != cfg.URL || p.ShadowConfig.URL != cfg.URL || p.BackfillConfig.URL != cfg.URL ||
		p.VirtualConfig.Target != cfg.Target || p.ShadowConfig.Target != cfg.Target || p.BackfillConfig.Target != cfg.Target ||
		p.VirtualConfig.Environment != cfg.Environment || p.ShadowConfig.Environment != cfg.Environment || p.BackfillConfig.Environment != cfg.Environment {
		return Status{}, fmt.Errorf("%w: pipeline database, target, or environment binding mismatch", ErrInvalid)
	}
	return Start(ctx, cfg, p.Spec, p.Actions())
}

func (p Pipeline) Actions() Actions {
	return Actions{
		Validate: func(context.Context) error {
			if err := p.Spec.Validate(); err != nil {
				return err
			}
			if p.Expand == nil {
				return fmt.Errorf("%w: verified expand executor required", ErrInvalid)
			}
			if err := p.Virtual.Validate(); err != nil {
				return err
			}
			if err := p.Shadow.Validate(p.ShadowPolicy); err != nil {
				return err
			}
			if p.Virtual.ArtifactDigest != p.Spec.ArtifactDigest || p.Shadow.ArtifactDigest != p.Spec.ArtifactDigest || p.Virtual.Previous.Name != p.Spec.PreviousVersion || p.Virtual.Current.Name != p.Spec.NewVersion {
				return fmt.Errorf("%w: pipeline artifact or version binding mismatch", ErrInvalid)
			}
			for _, s := range p.Backfills {
				if err := s.Validate(); err != nil {
					return err
				}
				if s.ArtifactDigest != p.Spec.ArtifactDigest {
					return fmt.Errorf("%w: backfill artifact binding mismatch", ErrInvalid)
				}
			}
			return nil
		},
		Expand: p.Expand,
		Compatibility: func(ctx context.Context) error {
			_, err := shadowsync.Apply(ctx, p.ShadowConfig, p.Shadow, p.ShadowPolicy)
			return err
		},
		Backfill: func(ctx context.Context) error {
			for _, s := range p.Backfills {
				if _, err := backfill.Run(ctx, p.BackfillConfig, s); err != nil {
					return err
				}
			}
			return nil
		},
		Publish: func(ctx context.Context) error {
			_, err := virtualschema.Apply(ctx, p.VirtualConfig, p.Virtual)
			return err
		},
	}
}
