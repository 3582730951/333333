package api

import "context"

// DeploymentHandoffSignal lets resumable admin streams finish a release drain
// without pinning the old worker. Business streams never receive this signal.
type DeploymentHandoffSignal struct {
	Done          <-chan struct{}
	TargetRelease func() string
}

type deploymentHandoffContextKey struct{}

func ContextWithDeploymentHandoff(ctx context.Context, signal DeploymentHandoffSignal) context.Context {
	if ctx == nil || signal.Done == nil {
		return ctx
	}
	return context.WithValue(ctx, deploymentHandoffContextKey{}, signal)
}

func DeploymentHandoffFromContext(ctx context.Context) (DeploymentHandoffSignal, bool) {
	if ctx == nil {
		return DeploymentHandoffSignal{}, false
	}
	signal, ok := ctx.Value(deploymentHandoffContextKey{}).(DeploymentHandoffSignal)
	return signal, ok && signal.Done != nil
}
