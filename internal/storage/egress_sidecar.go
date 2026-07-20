package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const CurlCFFISidecarEgressType = "curl_cffi_sidecar"

// IsSidecarEgress reports whether an egress directly speaks the curl_cffi sidecar
// protocol. It also returns true for a request-scoped account binding composed by
// WrapEgressWithSidecar.
func IsSidecarEgress(egress EgressProfile) bool {
	return strings.EqualFold(strings.TrimSpace(egress.Type), CurlCFFISidecarEgressType)
}

// WrapEgressWithSidecar composes two independently managed layers:
//
//	sidecar endpoint -> selected HTTP/SOCKS/WARP proxy -> provider upstream
//
// The returned profile deliberately keeps the selected egress ID, health, region,
// concurrency and exit IP. This ensures scheduler load, affinity, CF cooldowns and
// diagnostics remain attributed to the real network exit instead of the local
// sidecar process. The original transport is retained only in non-serialized fields
// so ClaudeForceDirect can explicitly bypass the sidecar without losing the chosen
// proxy.
func WrapEgressWithSidecar(egress, sidecar EgressProfile) (EgressProfile, error) {
	if !IsSidecarEgress(sidecar) {
		return EgressProfile{}, errors.New("bound sidecar egress must have type curl_cffi_sidecar")
	}
	if strings.TrimSpace(sidecar.Endpoint) == "" {
		return EgressProfile{}, errors.New("bound sidecar egress endpoint required")
	}
	// A primary/standby that is already a sidecar has a complete endpoint + optional
	// chain proxy. Do not nest one sidecar protocol inside another.
	if IsSidecarEgress(egress) {
		return egress, nil
	}

	wrapped := egress
	wrapped.TransportSidecarID = strings.TrimSpace(sidecar.ID)
	wrapped.TransportSidecarMaxConcurrency = sidecar.MaxConcurrency
	wrapped.TransportBaseType = egress.Type
	wrapped.TransportBaseURL = egress.Endpoint
	wrapped.TransportBaseChain = egress.ChainProxy
	wrapped.Type = CurlCFFISidecarEgressType
	wrapped.Endpoint = strings.TrimSpace(sidecar.Endpoint)
	wrapped.ChainProxy = strings.TrimSpace(sidecar.ChainProxy)

	// Non-direct egress profiles carry their actual proxy URL in Endpoint. That URL
	// takes precedence over a sidecar profile's own default chain because it is the
	// account's explicitly selected IP exit.
	baseType := strings.ToLower(strings.TrimSpace(egress.Type))
	if baseType != "" && baseType != "direct" {
		if endpoint := strings.TrimSpace(egress.Endpoint); endpoint != "" {
			wrapped.ChainProxy = endpoint
		}
	}
	return wrapped, nil
}

// ApplySidecarEgressBinding resolves and applies an account's optional sidecar
// wrapper to an already-selected primary or standby egress. A missing/invalid
// explicit wrapper is an error: callers must fail closed instead of leaking a Go
// stdlib TLS/HTTP2 fingerprint through the base egress.
func (s *Store) ApplySidecarEgressBinding(ctx context.Context, binding AccountEgressBinding, egress EgressProfile) (EgressProfile, error) {
	sidecarID := strings.TrimSpace(binding.SidecarEgressID)
	if sidecarID == "" || IsSidecarEgress(egress) {
		return egress, nil
	}
	sidecar, err := s.GetEgressProfile(ctx, sidecarID)
	if err != nil {
		return EgressProfile{}, fmt.Errorf("bound sidecar egress %q: %w", sidecarID, err)
	}
	wrapped, err := WrapEgressWithSidecar(egress, sidecar)
	if err != nil {
		return EgressProfile{}, fmt.Errorf("bound sidecar egress %q: %w", sidecarID, err)
	}
	return wrapped, nil
}

// ResolvePrimaryEgressBinding loads the binding's primary IP egress and applies
// its optional sidecar transport. Background auth/model/quota probes use this so
// they follow the same fingerprint-safe path as scheduled inference requests.
func (s *Store) ResolvePrimaryEgressBinding(ctx context.Context, binding AccountEgressBinding) (EgressProfile, error) {
	egress, err := s.GetEgressProfile(ctx, binding.PrimaryEgressID)
	if err != nil {
		return EgressProfile{}, err
	}
	return s.ApplySidecarEgressBinding(ctx, binding, egress)
}

// WithoutSidecarTransport restores the real selected egress for the explicit
// claude_force_direct escape hatch. A legacy profile that is itself a sidecar has
// no underlying proxy metadata and therefore preserves the historical direct
// fallback behavior.
func WithoutSidecarTransport(egress EgressProfile) EgressProfile {
	if strings.TrimSpace(egress.TransportSidecarID) == "" {
		if IsSidecarEgress(egress) {
			egress.Type = "direct"
			egress.Endpoint = ""
			egress.ChainProxy = ""
		}
		return egress
	}
	egress.Type = egress.TransportBaseType
	egress.Endpoint = egress.TransportBaseURL
	egress.ChainProxy = egress.TransportBaseChain
	egress.TransportSidecarID = ""
	egress.TransportSidecarMaxConcurrency = 0
	egress.TransportBaseType = ""
	egress.TransportBaseURL = ""
	egress.TransportBaseChain = ""
	return egress
}
