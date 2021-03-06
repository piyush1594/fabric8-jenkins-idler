package main

import (
	"os"

	"context"
	"github.com/fabric8-services/fabric8-jenkins-idler/internal/cluster"
	"github.com/fabric8-services/fabric8-jenkins-idler/internal/configuration"
	"github.com/fabric8-services/fabric8-jenkins-idler/internal/tenant"
	"github.com/fabric8-services/fabric8-jenkins-idler/internal/toggles"
	"github.com/fabric8-services/fabric8-jenkins-idler/internal/token"
	"github.com/fabric8-services/fabric8-jenkins-idler/internal/version"
	log "github.com/sirupsen/logrus"
)

var mainLogger = log.WithFields(log.Fields{"component": "main"})

func init() {
	log.SetFormatter(&log.JSONFormatter{})

	level := log.InfoLevel
	switch levelStr, _ := os.LookupEnv("JC_LOG_LEVEL"); levelStr {
	case "info":
		level = log.InfoLevel
	case "debug":
		level = log.DebugLevel
	case "warning":
		level = log.WarnLevel
	case "error":
		level = log.ErrorLevel
	default:
		level = log.InfoLevel
	}
	log.SetLevel(level)
}

func main() {
	mainLogger.Infof("Idler version: %s", version.GetVersion())

	// Init configuration
	config := createAndValidateConfiguration()
	mainLogger.Infof("Idler configuration: %s", config.String())

	// Get OSIO service account token from Auth
	osioToken := osioToken(config)

	// Get the view over the clusters
	clusterView := clusterView(osioToken, config)
	mainLogger.Infof("Cluster view: %s", clusterView.String())

	// Create Toggle (Unleash) Service
	featuresService := createFeatureToggle(config)

	// Create Tenant Service
	tenantService := tenant.NewTenantService(config.GetTenantURL(), osioToken)

	idler := NewIdler(featuresService, tenantService, clusterView, config)
	idler.Run()
}

func createAndValidateConfiguration() configuration.Configuration {
	config, err := configuration.NewConfiguration()
	if err != nil {
		log.Fatal(err)
	}

	multiError := config.Verify()
	if !multiError.Empty() {
		for _, err := range multiError.Errors {
			log.Error(err)
		}
		os.Exit(1)
	}
	return config
}

func createFeatureToggle(config configuration.Configuration) toggles.Features {
	var err error
	var features toggles.Features
	if len(config.GetFixedUuids()) > 0 {
		mainLogger.Infof("Using fixed UUID list for toggle feature: %s", config.GetFixedUuids())
		features, err = toggles.NewFixedUUIDToggle(config.GetFixedUuids())
	} else {
		features, err = toggles.NewUnleashToggle(config.GetToggleURL())
	}
	if err != nil {
		// Fatal with exit program
		mainLogger.WithField("err", err).Fatal("Unable to create feature toggles")
	}
	return features
}

func osioToken(config configuration.Configuration) string {
	osioToken, err := token.GetServiceAccountToken(config)
	if err != nil {
		// Fatal with exit program
		mainLogger.WithField("err", err).Fatal("Unable to retrieve service account token")
	}
	return osioToken
}

func clusterView(osioToken string, config configuration.Configuration) cluster.View {
	resolveToken := token.NewResolve(config.GetAuthURL())
	clusterService := cluster.NewService(
		config.GetAuthURL(),
		osioToken,
		resolveToken,
		token.NewPGPDecrypter(config.GetAuthTokenKey()),
	)
	clusterView, err := clusterService.GetClusterView(context.Background())
	if err != nil {
		// Fatal with exit program
		mainLogger.WithField("err", err).Fatal("Unable to resolve cluster view")
	}

	return clusterView
}
