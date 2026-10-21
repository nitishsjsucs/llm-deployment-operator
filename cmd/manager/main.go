/*
Copyright 2026 The llm-deployment-operator Authors.

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

// Command manager runs the llm-deployment-operator controllers.
package main

import (
	"context"
	"flag"
	"os"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	servingv1alpha1 "github.com/nitishsjsucs/llm-deployment-operator/api/v1alpha1"
	"github.com/nitishsjsucs/llm-deployment-operator/internal/controller"
	"github.com/nitishsjsucs/llm-deployment-operator/internal/render"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(servingv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr        string
		probeAddr          string
		leaderElect        bool
		inferencePoolGroup string
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the Prometheus metrics endpoint binds to. Use 0 to disable it.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the health probe endpoint binds to.")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election so that only one manager is active.")
	flag.StringVar(&inferencePoolGroup, "inference-pool-api-group", render.DefaultInferencePoolGroup,
		"API group of the InferencePools KServe creates (inference.networking.k8s.io or inference.networking.x-k8s.io).")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Only cache the ControllerRevisions this operator owns, not those of
	// every StatefulSet and DaemonSet in the cluster.
	owned, err := labels.NewRequirement(render.LabelDeployment, selection.Exists, nil)
	if err != nil {
		setupLog.Error(err, "building label selector")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "llm-deployment-operator.serving.llmops.dev",
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&appsv1.ControllerRevision{}: {Label: labels.NewSelector().Add(*owned)},
			},
		},
		// LLMInferenceServices and HTTPRoutes are handled as unstructured
		// objects; serve their reads from the informers the watches start.
		Client: client.Options{Cache: &client.CacheOptions{Unstructured: true}},
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()
	if err := setup(ctx, mgr, inferencePoolGroup); err != nil {
		setupLog.Error(err, "unable to set up controllers")
		os.Exit(1)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func setup(ctx context.Context, mgr ctrl.Manager, inferencePoolGroup string) error {
	if err := controller.SetupIndexes(ctx, mgr.GetFieldIndexer()); err != nil {
		return err
	}
	if err := (&controller.ModelDeploymentReconciler{
		Client:             mgr.GetClient(),
		Scheme:             mgr.GetScheme(),
		Recorder:           mgr.GetEventRecorderFor("llm-deployment-operator"),
		Clock:              clock.RealClock{},
		InferencePoolGroup: inferencePoolGroup,
	}).SetupWithManager(mgr); err != nil {
		return err
	}
	return (&controller.TenantQuotaReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr)
}
