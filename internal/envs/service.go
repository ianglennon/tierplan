// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"context"
	"errors"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/store"
)

// Errors of the registry.
var (
	ErrSocketNotFound    = errors.New("envs: no Docker socket was found")
	ErrEngineUnreachable = errors.New("envs: the Docker engine did not answer")
	ErrUnsupportedEngine = errors.New("envs: the Docker engine is not supported")
	ErrLocalExists       = errors.New("envs: the local host is already an environment")
	ErrInvalidName       = errors.New("envs: the name must be 1 to 64 characters")
	ErrNotFound          = errors.New("envs: no such environment")
)

// Environment is a managed environment with its live status.
type Environment struct {
	ID, Kind, Name, SocketPath string
	Enabled                    bool
	CreatedAt                  time.Time
	Status                     Status
}

// Service is the environment registry.
type Service struct{}

// NewService returns the registry.
func NewService(s *store.Store, w *audit.Writer, m *Monitor, dockerHost string, selfID string, now func() time.Time) *Service {
	return &Service{}
}

// List returns every environment.
func (s *Service) List(ctx context.Context) ([]Environment, error) { return nil, errNotImplemented }

// Get returns one environment.
func (s *Service) Get(ctx context.Context, id string) (Environment, error) {
	return Environment{}, errNotImplemented
}

// AddLocal opts the local Docker host in.
func (s *Service) AddLocal(ctx context.Context, actor audit.Actor, name string, allowUnsupported bool, ip, ua string) (Environment, error) {
	return Environment{}, errNotImplemented
}

// Rename renames an environment.
func (s *Service) Rename(ctx context.Context, actor audit.Actor, id, name, ip, ua string) (Environment, error) {
	return Environment{}, errNotImplemented
}

// Remove removes an environment.
func (s *Service) Remove(ctx context.Context, actor audit.Actor, id, ip, ua string) error {
	return errNotImplemented
}

// Reconnect asks for an immediate probe.
func (s *Service) Reconnect(ctx context.Context, id string) (Environment, error) {
	return Environment{}, errNotImplemented
}

// StartAll starts the monitor of every stored environment.
func (s *Service) StartAll(ctx context.Context) error { return errNotImplemented }

// HasLocal reports whether the local host is an environment.
func (s *Service) HasLocal(ctx context.Context) (bool, error) { return false, errNotImplemented }

// SelfContainerID is the ID of Bayplan's own container, or empty.
func (s *Service) SelfContainerID() string { return "" }

// Resolve implements httpx.EnvResolver.
func (s *Service) Resolve(ctx context.Context, envID string) (httpx.Env, error) {
	return httpx.Env{}, errNotImplemented
}

var _ httpx.EnvResolver = (*Service)(nil)
