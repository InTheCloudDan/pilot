// Copyright 2017 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"fmt"
	"os"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/golang/glog"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/spf13/cobra"

	proxyconfig "istio.io/api/proxy/v1/config"
	"istio.io/pilot/adapter/config/aggregate"
	"istio.io/pilot/adapter/config/ingress"
	"istio.io/pilot/adapter/config/tpr"
	"istio.io/pilot/adapter/config/memory"
	"istio.io/pilot/cmd"
	"istio.io/pilot/model"
	"istio.io/pilot/platform/kube"
	"istio.io/pilot/platform/vms"
	"istio.io/pilot/proxy"
	"istio.io/pilot/proxy/envoy"
	"istio.io/pilot/tools/version"

	vmsclient "github.com/amalgam8/amalgam8/registry/client"
	vmsconfig "github.com/amalgam8/amalgam8/sidecar/config"
)

// Adapter defines options for underlying platform
type Adapter string

const (
	KubernetesAdapter Adapter = "Kubernetes"
	VMsAdapter        Adapter = "VMs"
)

// store the args related to VMs configuration
type VMsArgs struct {
	config    string
	serverURL string
	authToken string
}

type args struct {
	kubeconfig string
	meshconfig string

	// ingress sync mode is set to off by default
	controllerOptions kube.ControllerOptions
	discoveryOptions  envoy.DiscoveryServiceOptions

	adapter Adapter
	vmsArgs VMsArgs
}

var (
	flags args

	rootCmd = &cobra.Command{
		Use:   "pilot",
		Short: "Istio Pilot",
		Long:  "Istio Pilot provides management plane functionality to the Istio service mesh and Istio Mixer.",
	}

	discoveryCmd = &cobra.Command{
		Use:   "discovery",
		Short: "Start Istio proxy discovery service",
		RunE: func(c *cobra.Command, args []string) error {
			if flags.adapter == "" {
				flags.adapter = KubernetesAdapter
			}

			if flags.adapter == KubernetesAdapter {
				client, err := kube.CreateInterface(flags.kubeconfig)
				if err != nil {
					return multierror.Prefix(err, "failed to connect to Kubernetes API.")
				}

				if flags.controllerOptions.Namespace == "" {
					flags.controllerOptions.Namespace = os.Getenv("POD_NAMESPACE")
				}

				glog.V(2).Infof("version %s", version.Line())
				glog.V(2).Infof("flags %s", spew.Sdump(flags))

				// receive mesh configuration
				mesh, err := cmd.ReadMeshConfig(flags.meshconfig)
				if err != nil {
					return multierror.Prefix(err, "failed to read mesh configuration.")
				}

				glog.V(2).Infof("mesh configuration %s", spew.Sdump(mesh))

				tprClient, err := tpr.NewClient(flags.kubeconfig, model.ConfigDescriptor{
					model.RouteRuleDescriptor,
					model.DestinationPolicyDescriptor,
				}, flags.controllerOptions.Namespace)
				if err != nil {
					return multierror.Prefix(err, "failed to open a TPR client")
				}

				if err = tprClient.RegisterResources(); err != nil {
					return multierror.Prefix(err, "failed to register Third-Party Resources.")
				}

				serviceController := kube.NewController(client, mesh, flags.controllerOptions)
				var configController model.ConfigStoreCache
				if mesh.IngressControllerMode == proxyconfig.ProxyMeshConfig_OFF {
					configController = tpr.NewController(tprClient, flags.controllerOptions.ResyncPeriod)
				} else {
					configController, err = aggregate.MakeCache([]model.ConfigStoreCache{
						tpr.NewController(tprClient, flags.controllerOptions.ResyncPeriod),
						ingress.NewController(client, mesh, flags.controllerOptions),
					})
					if err != nil {
						return err
					}
				}

				environment := proxy.Environment{
					ServiceDiscovery: serviceController,
					ServiceAccounts:  serviceController,
					IstioConfigStore: model.MakeIstioStore(configController),
					SecretRegistry:   kube.MakeSecretRegistry(client),
					Mesh:             mesh,
				}
				discovery, err := envoy.NewDiscoveryService(serviceController, configController, environment, flags.discoveryOptions)
				if err != nil {
					return fmt.Errorf("failed to create discovery service: %v", err)
				}

				ingressSyncer := ingress.NewStatusSyncer(mesh, client, flags.controllerOptions)

				stop := make(chan struct{})
				go serviceController.Run(stop)
				go configController.Run(stop)
				go discovery.Run()
				go ingressSyncer.Run(stop)
				cmd.WaitSignal(stop)
			} else if flags.adapter == VMsAdapter {
				vmsConfig := *&vmsconfig.DefaultConfig
				if flags.vmsArgs.config != "" {
					err := vmsConfig.LoadFromFile(flags.vmsArgs.config)
					if err != nil {
						return multierror.Prefix(err, "failed to read vms config file.")
					}
				}
				if flags.vmsArgs.serverURL != "" {
					vmsConfig.A8Registry.URL = flags.vmsArgs.serverURL
				}
				if flags.vmsArgs.authToken != "" {
					vmsConfig.A8Registry.Token = flags.vmsArgs.authToken
				}

				mesh := proxy.DefaultMeshConfig()

				vmsClient, err := vmsclient.New(vmsclient.Config{
					URL:       vmsConfig.A8Registry.URL,
					AuthToken: vmsConfig.A8Registry.Token,
				})
				if err != nil {
					return multierror.Prefix(err, "failed to create VMs client.")
				}
				serviceController := vms.NewController(vms.ControllerConfig{
					Discovery: vmsClient,
					Mesh:      &mesh,
				})
				configController := memory.NewController(memory.Make(model.ConfigDescriptor{
					model.RouteRuleDescriptor,
					model.DestinationPolicyDescriptor,
				}))

				environment := proxy.Environment{
					ServiceDiscovery: serviceController,
					ServiceAccounts:  serviceController,
					IstioConfigStore: model.MakeIstioStore(configController),
					Mesh:             &mesh,
				}
				discovery, err := envoy.NewDiscoveryService(serviceController, configController, environment, flags.discoveryOptions)
				if err != nil {
					return fmt.Errorf("failed to create discovery service: %v", err)
				}

				stop := make(chan struct{})
				go serviceController.Run(stop)
				go configController.Run(stop)
				go discovery.Run()
				cmd.WaitSignal(stop)
			}

			return nil
		},
	}
)

func init() {
	discoveryCmd.PersistentFlags().StringVar((*string)(&flags.adapter), "adapter", string(KubernetesAdapter),
		fmt.Sprintf("Select the underlying running platform, options are {%s, %s}", string(KubernetesAdapter), string(VMsAdapter)))
	discoveryCmd.PersistentFlags().StringVar(&flags.kubeconfig, "kubeconfig", "",
		"Use a Kubernetes configuration file instead of in-cluster configuration")
	discoveryCmd.PersistentFlags().StringVar(&flags.meshconfig, "meshConfig", "/etc/istio/config/mesh",
		fmt.Sprintf("File name for Istio mesh configuration"))
	discoveryCmd.PersistentFlags().StringVarP(&flags.controllerOptions.Namespace, "namespace", "n", "",
		"Select a namespace for the controller loop. If not set, uses ${POD_NAMESPACE} environment variable")
	discoveryCmd.PersistentFlags().DurationVar(&flags.controllerOptions.ResyncPeriod, "resync", time.Second,
		"Controller resync interval")
	discoveryCmd.PersistentFlags().StringVar(&flags.controllerOptions.DomainSuffix, "domain", "cluster.local",
		"DNS domain suffix")

	discoveryCmd.PersistentFlags().IntVar(&flags.discoveryOptions.Port, "port", 8080,
		"Discovery service port")
	discoveryCmd.PersistentFlags().BoolVar(&flags.discoveryOptions.EnableProfiling, "profile", true,
		"Enable profiling via web interface host:port/debug/pprof")
	discoveryCmd.PersistentFlags().BoolVar(&flags.discoveryOptions.EnableCaching, "discovery_cache", true,
		"Enable caching discovery service responses")

	discoveryCmd.PersistentFlags().StringVar(&flags.vmsArgs.config, "vmsconfig", "",
		"VMs Config file for discovery")
	discoveryCmd.PersistentFlags().StringVar(&flags.vmsArgs.serverURL, "serverURL", "",
		"URL for the registry server")

	discoveryCmd.PersistentFlags().StringVar(&flags.vmsArgs.config, "authToken", "",
		"Authorization token used to access the registry server")
	cmd.AddFlags(rootCmd)

	rootCmd.AddCommand(discoveryCmd)
	rootCmd.AddCommand(cmd.VersionCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		glog.Error(err)
		os.Exit(-1)
	}
}
