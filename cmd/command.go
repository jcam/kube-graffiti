/*
Copyright (C) 2018 Expedia Group.
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

package cmd

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/Telefonica/kube-graffiti/pkg/config"
	"github.com/Telefonica/kube-graffiti/pkg/existing"
	"github.com/Telefonica/kube-graffiti/pkg/graffiti"
	"github.com/Telefonica/kube-graffiti/pkg/healthcheck"
	"github.com/Telefonica/kube-graffiti/pkg/log"
	"github.com/Telefonica/kube-graffiti/pkg/webhook"
	"github.com/mitchellh/mapstructure"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	// DefaultLogLevel - the zero logging level set for whole program
	DefaultLogLevel   = "info"
	defaultConfigPath = "/config"
)

var (
	componentName = "cmd"
	cfgFile       string
	rootCmd       = &cobra.Command{
		Use:     "kube-graffiti",
		Short:   "Automatically add labels and/or annotations to kubernetes objects",
		Long:    `Write rules that match labels and object fields and add labels/annotations to kubernetes objects as they are created via a mutating webhook.`,
		Example: `kube-graffiti --config ./config.yaml`,
		PreRun:  initRootCmd,
		Run:     runRootCmd,
	}
)

// init defines command-line and environment arguments
func init() {
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "/config", "[GRAFFITI_CONFIG] config file (default is /config.{yaml,json,toml,hcl})")
	viper.BindPFlag("config", rootCmd.PersistentFlags().Lookup("config"))
	rootCmd.PersistentFlags().String("log-level", DefaultLogLevel, "[GRAFFITI_LOG_LEVEL] set logging verbosity to one of panic, fatal, error, warn, info, debug")
	viper.BindPFlag("log-level", rootCmd.PersistentFlags().Lookup("log-level"))
	// viper.BindEnv("log-level", "GRAFFITI_LOG_LEVEL")
	rootCmd.PersistentFlags().Bool("check-existing", false, "[GRAFFITI_CHECK_EXISTING] run rules against existing objects")
	viper.BindPFlag("check-existing", rootCmd.PersistentFlags().Lookup("check-existing"))

	// set up Viper environment variable binding...
	replacer := strings.NewReplacer("-", "_", ".", "_")
	viper.SetEnvPrefix("GRAFFITI")
	viper.SetEnvKeyReplacer(replacer)
	viper.AutomaticEnv()
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func initRootCmd(_ *cobra.Command, _ []string) {
	log.InitLogger(viper.GetString("log-level"))
}

// runRootCmd is the main program which starts up our services and waits for them to complete
func runRootCmd(_ *cobra.Command, _ []string) {
	mylog := log.ComponentLogger(componentName, "runRootCmd")

	mylog.Info().Str("file", viper.GetString("config")).Msg("reading configuration file")
	config, err := loadConfig(viper.GetString("config"))
	if err != nil {
		mylog.Fatal().Err(err).Msg("failed to load config")
	}

	mylog.Info().Str("level", viper.GetString("log-level")).Msg("Setting log-level to configured level")
	log.ChangeLogLevel(viper.GetString("log-level"))
	mylog = log.ComponentLogger(componentName, "runRootCmd")
	mylog.Info().Str("log-level", viper.GetString("log-level")).Msg("This is the log level")

	mylog.Info().Msg("configuration read ok")
	mylog.Debug().Msg("validating config")
	if err := config.ValidateConfig(); err != nil {
		mylog.Fatal().Err(err).Msg("failed to validate config")
	}

	mylog.Debug().Msg("getting kubernetes client")
	kubeClient, restConfig := getKubeClients()
	// Setup and start the health-checker
	healthChecker := healthcheck.NewHealthChecker(healthcheck.NewCutDownNamespaceClient(kubeClient), viper.GetInt("health-checker.port"), viper.GetString("health-checker.path"))
	healthChecker.StartHealthChecker()

	// Setup and start the mutating webhook server
	if err := initWebhookServer(config, kubeClient); err != nil {
		mylog.Fatal().Err(err).Msg("webhook server failed to start")
	}

	if err := initExistingCheck(config, restConfig); err != nil {
		mylog.Fatal().Err(err).Msg("failed to check existing namespaces")
	}

	// wait for an interrupt
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, os.Kill)
	<-signalChan
	os.Exit(0)
}

// getKubeClients returns client-go clientset and a dynamic client
func getKubeClients() (*kubernetes.Clientset, *rest.Config) {
	mylog := log.ComponentLogger(componentName, "getKubeClients")
	// creates the in-cluster config
	mylog.Info().Msg("creating kubeconfig")
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}

	// creates the clientset
	mylog.Debug().Msg("creating kubernetes api clientset")
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}
	return client, config
}

func initWebhookServer(c config.Configuration, k *kubernetes.Clientset) error {
	mylog := log.ComponentLogger(componentName, "initWebhookServer")
	port := viper.GetInt("server.port")

	mylog.Debug().Int("port", port).Msg("creating a new webhook server")
	caPath := viper.GetString("server.ca-cert-path")
	ca, err := ioutil.ReadFile(caPath)
	if err != nil {
		mylog.Error().Err(err).Str("path", caPath).Msg("Failed to load ca from file")
		return errors.New("failed to load ca from file")
	}
	mylog.Debug().Str("ca-cert-path", caPath).Msg("loaded ca cert ok")
	server := webhook.NewServer(
		viper.GetString("server.company-domain"),
		viper.GetString("server.namespace"),
		viper.GetString("server.service"),
		ca, k,
		viper.GetInt("server.port"),
	)

	// add each of the graffiti rules into the mux
	mylog.Info().Int("count", len(c.Rules)).Msg("loading graffiti rules")
	for _, rule := range c.Rules {
		mylog.Info().Str("rule-name", rule.Registration.Name).Msg("adding graffiti rule")
		server.AddGraffitiRule(graffiti.Rule{
			Name:     rule.Registration.Name,
			Matchers: rule.Matchers,
			Payload:  rule.Payload,
		})
	}

	mylog.Info().Int("port", port).Str("server.cert-path", viper.GetString("server.cert-path")).Str("server.key-path", viper.GetString("server.key-path")).Msg("starting webhook secure webserver")
	server.StartWebhookServer(viper.GetString("server.cert-path"), viper.GetString("server.key-path"))

	mylog.Debug().Msg("waiting 2 seconds")
	time.Sleep(2 * time.Second)

	// register all rules with the kubernetes apiserver
	for _, rule := range c.Rules {
		mylog.Info().Str("name", rule.Registration.Name).Msg("registering rule with api server")
		err = server.RegisterHook(rule.Registration, k)
		if err != nil {
			mylog.Error().Err(err).Str("name", rule.Registration.Name).Msg("failed to register rule with apiserver")
			return err
		}
	}

	return nil
}

func initExistingCheck(config config.Configuration, r *rest.Config) error {
	mylog := log.ComponentLogger(componentName, "initExistingCheck")

	var err error
	if !viper.IsSet("check-existing") || viper.GetString("check-existing") != "true" {
		mylog.Info().Msg("checking of existing objects is disabled")
		return nil
	}
	if err = existing.InitKubeClients(r); err != nil {
		return err
	}
	existing.ApplyRulesAgainstExistingObjects(config.Rules)

	mylog.Info().Msg("check of existing objects completed successfully")

	return nil
}

// LoadConfig is reponsible for loading the viper configuration file.
func loadConfig(file string) (config.Configuration, error) {
	setDefaults()

	// Don't forget to read config either from cfgFile or from home directory!
	if file != "" {
		// Use config file from the flag.
		viper.SetConfigFile(file)
	} else {
		viper.SetConfigName(defaultConfigPath)
	}

	if err := viper.ReadInConfig(); err != nil {
		fmt.Println("Can't read config:", err)
		os.Exit(1)
	}

    viper.Debug()
	return unmarshalFromViperStrict()
}

func setDefaults() {
	viper.SetDefault("log-level", DefaultLogLevel)
	viper.SetDefault("check-existing", false)
	viper.SetDefault("server.port", 8443)
	viper.SetDefault("health-checker.port", 8080)
	viper.SetDefault("health-checker.path", "/healthz")
	viper.SetDefault("server.company-domain", "acme.com")
	viper.SetDefault("server.ca-cert-path", "/ca-cert")
	viper.SetDefault("server.cert-path", "/server-cert")
	viper.SetDefault("server.key-path", "/server-key")
}

func unmarshalFromViperStrict() (config.Configuration, error) {
    var c config.Configuration

	// add in a special decoder so that viper can unmarshal boolean operator values such as AND, OR and XOR
	// and enable mapstructure's ErrorUnused checking so we can catch bad configuration keys in the source.
	decoderHookFunc := mapstructure.ComposeDecodeHookFunc(
		mapstructure.StringToTimeDurationHookFunc(),
		mapstructure.StringToSliceHookFunc(","),
		graffiti.StringToBooleanOperatorFunc(),
	)
	opts := decodeHookWithErrorUnused(decoderHookFunc)

	if err := viper.UnmarshalKey("server", &c.Server, opts); err != nil {
		return c, fmt.Errorf("failed to unmarshal server: %v", err)
	}
	if err := viper.UnmarshalKey("health-check", &c.HealthChecker, opts); err != nil {
		return c, fmt.Errorf("failed to unmarshal health-check: %v", err)
	}
	if err := viper.UnmarshalKey("rules", &c.Rules, opts); err != nil {
		return c, fmt.Errorf("failed to unmarshal rules: %v", err)
	}
    c.LogLevel = viper.GetString("log-level")
    if !viper.IsSet("check-existing") || viper.GetString("check-existing") != "true" {
        c.CheckExisting = false
    } else {
        c.CheckExisting = true
    }

	//if err := viper.Unmarshal(&c2, opts); err != nil {
	//	return c2, fmt.Errorf("failed to unmarshal configuration: %v", err)
	//}
	return c, nil
}

// Our own implementation of Viper's DecodeHook so that we can set ErrorUnused to true
func decodeHookWithErrorUnused(hook mapstructure.DecodeHookFunc) viper.DecoderConfigOption {
	return func(c *mapstructure.DecoderConfig) {
		c.DecodeHook = hook
		c.ErrorUnused = true
	}
}
