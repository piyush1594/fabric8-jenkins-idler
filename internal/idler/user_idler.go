package idler

import (
	"context"
	"sync"
	"time"

	"fmt"

	"github.com/fabric8-services/fabric8-jenkins-idler/internal/condition"
	"github.com/fabric8-services/fabric8-jenkins-idler/internal/configuration"
	"github.com/fabric8-services/fabric8-jenkins-idler/internal/model"
	"github.com/fabric8-services/fabric8-jenkins-idler/internal/openshift/client"
	"github.com/fabric8-services/fabric8-jenkins-idler/internal/toggles"
	log "github.com/sirupsen/logrus"
)

var logger = log.WithFields(log.Fields{"component": "user-idler"})

// JenkinsServices is an array of all the services getting idled or unidled
// they go along the main build detection logic of jenkins and don't have
// any specific scenarios.
var JenkinsServices = []string{"jenkins", "content-repository"}

const (
	bufferSize             = 10
	jenkinsNamespaceSuffix = "-jenkins"
	jenkinsServiceName     = "jenkins"
)

// UserIdler is created for each monitored user/namespace.
// Each UserIdler runs in its own goroutine. The task of the UserIdler is to keep track
// of the Jenkins instance of the user and idle resp. un-idle depending on the evaluation
// of the given conditions for this UserIdler.
type UserIdler struct {
	openShiftAPI         string
	openShiftBearerToken string
	openShiftClient      client.OpenShiftClient
	maxRetries           int
	idleAttempts         int
	unIdleAttempts       int
	Conditions           *condition.Conditions
	logger               *log.Entry
	userChan             chan model.User
	user                 model.User
	config               configuration.Configuration
	features             toggles.Features
}

// NewUserIdler creates an instance of UserIdler.
// It returns a pointer to UserIdler,
func NewUserIdler(user model.User, openShiftAPI string, openShiftBearerToken string, config configuration.Configuration, features toggles.Features) *UserIdler {
	logEntry := log.WithFields(log.Fields{
		"component": "user-idler",
		"username":  user.Name,
		"id":        user.ID,
	})
	logEntry.Info("UserIdler created.")

	conditions := createWatchConditions(config.GetProxyURL(), config.GetIdleAfter(), logEntry)

	userChan := make(chan model.User, bufferSize)

	userIdler := UserIdler{
		openShiftAPI:         openShiftAPI,
		openShiftBearerToken: openShiftBearerToken,
		openShiftClient:      client.NewOpenShift(),
		maxRetries:           config.GetMaxRetries(),
		idleAttempts:         0,
		unIdleAttempts:       0,
		Conditions:           conditions,
		logger:               logEntry,
		userChan:             userChan,
		user:                 user,
		config:               config,
		features:             features,
	}
	return &userIdler
}

// GetUser returns the model.User of this idler.
func (idler *UserIdler) GetUser() model.User {
	return idler.user
}

// GetChannel gets channel of model.User type of this UserIdler.
func (idler *UserIdler) GetChannel() chan model.User {
	return idler.userChan
}

// checkIdle verifies the state of conditions and decides if we should idle/unidle
// and performs the required action if needed.
func (idler *UserIdler) checkIdle() error {
	eval, errors := idler.Conditions.Eval(idler.user)
	if !errors.Empty() {
		return errors.ToError()
	}

	idler.logger.WithField("eval", eval).Debug("Check idle state")
	if eval {
		enabled, err := idler.isIdlerEnabled()
		if err != nil {
			return err
		}
		if enabled {
			idler.doIdle()
		}
	} else {
		idler.doUnIdle()
	}

	return nil
}

// Run runs/starts the Idler
// It checks if Jenkins is idle at every checkIdle duration.
func (idler *UserIdler) Run(ctx context.Context, wg *sync.WaitGroup, cancel context.CancelFunc, checkIdle time.Duration, maxRetriesQuietInterval time.Duration) {
	idler.logger.WithFields(log.Fields{"checkIdle": fmt.Sprintf("%.0fm", checkIdle.Minutes()), "maxRetriesQuietInterval": fmt.Sprintf("%.0fm", maxRetriesQuietInterval.Minutes())}).Info("UserIdler started.")
	wg.Add(1)
	go func() {
		ticker := time.Tick(maxRetriesQuietInterval)
		timer := time.After(checkIdle)
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				idler.logger.Info("Shutting down user idler.")
				cancel()
				return
			case idler.user = <-idler.userChan:
				idler.logger.WithField("state", idler.user.String()).Debug("Received user data.")

				err := idler.checkIdle()
				if err != nil {
					idler.logger.WithField("error", err.Error()).Warn("Error during idle check.")
				}
				// Resetting the timer
				timer = time.After(checkIdle)
			case <-timer:
				// Timer handles the case where there are no OpenShift events received for the user for the checkIdle
				// duration. This ensures checkIdle will be called regularly.
				idler.logger.WithField("state", idler.user.String()).Info("Time based idle check.")
				err := idler.checkIdle()
				if err != nil {
					idler.logger.WithField("error", err.Error()).Warn("Error during idle check.")
				}
			case <-ticker:
				// Using ticker for the resetting of counters to ensure it occurs
				idler.logger.Debug("Resetting retry counters.")
				idler.resetCounters()
			}
		}
	}()
}

func (idler *UserIdler) doIdle() error {
	if idler.idleAttempts >= idler.maxRetries {
		idler.logger.Warn("Skipping idle request since max retry count has been reached.")
		return nil
	}

	state, err := idler.getJenkinsState()
	if err != nil {
		return err
	}

	if state > model.JenkinsIdled {
		idler.incrementIdleAttempts()
		for _, service := range JenkinsServices {
			idler.logger.WithField("attempt", fmt.Sprintf("(%d/%d)", idler.idleAttempts, idler.maxRetries)).Info("About to idle " + service)
			err := idler.openShiftClient.Idle(idler.openShiftAPI, idler.openShiftBearerToken, idler.user.Name+jenkinsNamespaceSuffix, service)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (idler *UserIdler) doUnIdle() error {
	if idler.unIdleAttempts >= idler.maxRetries {
		idler.logger.Warn("Skipping un-idle request since max retry count has been reached.")
		return nil
	}

	state, err := idler.getJenkinsState()
	if err != nil {
		return err
	}

	if state == model.JenkinsIdled {
		idler.incrementUnIdleAttempts()
		for _, service := range JenkinsServices {
			idler.logger.WithField("attempt", fmt.Sprintf("(%d/%d)", idler.unIdleAttempts, idler.maxRetries)).Info("About to un-idle " + service)
			err := idler.openShiftClient.UnIdle(idler.openShiftAPI, idler.openShiftBearerToken, idler.user.Name+jenkinsNamespaceSuffix, service)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (idler *UserIdler) isIdlerEnabled() (bool, error) {
	enabled, err := idler.features.IsIdlerEnabled(idler.user.ID)
	if err != nil {
		return false, err
	}

	if enabled {
		logger.WithFields(log.Fields{"user": idler.user.Name, "uuid": idler.user.ID}).Debug("Idler enabled.")
		return true, nil
	}

	logger.WithFields(log.Fields{"user": idler.user.Name, "uuid": idler.user.ID}).Debug("Idler not enabled.")
	return false, nil
}

func (idler *UserIdler) getJenkinsState() (int, error) {
	ns := idler.user.Name + jenkinsNamespaceSuffix
	state, err := idler.openShiftClient.IsIdle(idler.openShiftAPI, idler.openShiftBearerToken, ns, jenkinsServiceName)
	if err != nil {
		return -1, err
	}
	return state, nil
}

func (idler *UserIdler) incrementIdleAttempts() {
	idler.idleAttempts++
}

func (idler *UserIdler) incrementUnIdleAttempts() {
	idler.unIdleAttempts++
}

func (idler *UserIdler) resetCounters() {
	idler.idleAttempts = 0
	idler.unIdleAttempts = 0
}

func createWatchConditions(proxyURL string, idleAfter int, logEntry *log.Entry) *condition.Conditions {
	conditions := condition.NewConditions()

	// Add a Build condition.
	conditions.Add("build", condition.NewBuildCondition(time.Duration(idleAfter)*time.Minute))

	// Add a DeploymentConfig condition.
	conditions.Add("DC", condition.NewDCCondition(time.Duration(idleAfter)*time.Minute))

	// If we have access to Proxy, add User condition.
	if len(proxyURL) > 0 {
		logEntry.Debug("Adding 'user' condition")
		conditions.Add("user", condition.NewUserCondition(proxyURL, time.Duration(idleAfter)*time.Minute))
	}

	return &conditions
}
