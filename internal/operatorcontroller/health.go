package operatorcontroller

import (
	"context"
	"errors"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

type RunOptions struct {
	LeaderElection         bool
	MetricsBindAddress     string
	HealthProbeBindAddress string
}

func (o RunOptions) withDefaults() RunOptions {
	if o.MetricsBindAddress == "" {
		o.MetricsBindAddress = ":8080"
	}
	if o.HealthProbeBindAddress == "" {
		o.HealthProbeBindAddress = ":8081"
	}
	return o
}

func setupHealth(mgr manager.Manager) error {
	if err := mgr.AddHealthzCheck("manager", healthz.Ping); err != nil {
		return err
	}
	return mgr.AddReadyzCheck("cache", cacheReady(mgr.GetCache().WaitForCacheSync))
}

// Cache synchronization is independent of leadership and schema-resource Ready
// conditions. Healthy standby managers must pass the same readiness probe.
func cacheReady(synced func(context.Context) bool) healthz.Checker {
	return func(request *http.Request) error {
		ctx, cancel := context.WithTimeout(request.Context(), time.Second)
		defer cancel()
		if !synced(ctx) {
			return errors.New("operator cache is not synchronized")
		}
		return nil
	}
}
