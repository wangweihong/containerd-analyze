/*
   Copyright The containerd Authors.

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

package namespaces

import (
	"context"
	"os"

	"github.com/containerd/containerd/errdefs"
	"github.com/pkg/errors"
)

const (
	// NamespaceEnvVar is the environment variable key name
	NamespaceEnvVar = "CONTAINERD_NAMESPACE"
	// Default is the name of the default namespace
	Default = "default"
)

type namespaceKey struct{}

// WithNamespace sets a given namespace on the context
func WithNamespace(ctx context.Context, namespace string) context.Context {
	ctx = context.WithValue(ctx, namespaceKey{}, namespace) // set our key for namespace

	// also store on the grpc headers so it gets picked up by any clients that
	// are using this.
	return withGRPCNamespaceHeader(ctx, namespace)
}

// NamespaceFromEnv uses the namespace defined in CONTAINERD_NAMESPACE or
// default
func NamespaceFromEnv(ctx context.Context) context.Context {
	namespace := os.Getenv(NamespaceEnvVar)
	if namespace == "" {
		namespace = Default
	}
	return WithNamespace(ctx, namespace)
}

// Namespace returns the namespace from the context.
//
// The namespace is not guaranteed to be valid.
// 从context提取命名空间信息
func Namespace(ctx context.Context) (string, bool) {
	namespace, ok := ctx.Value(namespaceKey{}).(string) //这是context.Context的标准用法
	if !ok {
		return fromGRPCHeader(ctx)
	}

	return namespace, ok
}

// NamespaceRequired returns the valid namepace from the context or an error.
func NamespaceRequired(ctx context.Context) (string, error) {
	namespace, ok := Namespace(ctx)
	if !ok || namespace == "" {
		return "", errors.Wrapf(errdefs.ErrFailedPrecondition, "namespace is required")
	}

	if err := Validate(namespace); err != nil {
		return "", errors.Wrap(err, "namespace validation")
	}

	return namespace, nil
}
