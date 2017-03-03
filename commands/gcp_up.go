package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"strconv"
	"strings"

	"github.com/cloudfoundry/bosh-bootloader/bosh"
	gcpcloudconfig "github.com/cloudfoundry/bosh-bootloader/cloudconfig/gcp"
	yaml "gopkg.in/yaml.v2"

	"github.com/cloudfoundry/bosh-bootloader/helpers"
	"github.com/cloudfoundry/bosh-bootloader/storage"
	"github.com/cloudfoundry/bosh-bootloader/terraform"
)

var (
	marshal = yaml.Marshal
)

const (
	DIRECTOR_USERNAME = "admin"
)

type GCPUp struct {
	stateStore              stateStore
	keyPairUpdater          keyPairUpdater
	gcpProvider             gcpProvider
	boshManager             boshManager
	logger                  logger
	boshClientProvider      boshClientProvider
	cloudConfigGenerator    gcpCloudConfigGenerator
	terraformOutputProvider terraformOutputProvider
	terraformExecutor       terraformExecutor
	zones                   zones
	envIDManager            envIDManager
}

type GCPUpConfig struct {
	ServiceAccountKeyPath string
	ProjectID             string
	Zone                  string
	Region                string
	OpsFilePath           string
	Name                  string
	NoDirector            bool
}

type gcpCloudConfigGenerator interface {
	Generate(gcpcloudconfig.CloudConfigInput) (gcpcloudconfig.CloudConfig, error)
}

type gcpKeyPairCreator interface {
	Create() (string, string, error)
}

type keyPairUpdater interface {
	Update() (storage.KeyPair, error)
}

type gcpProvider interface {
	SetConfig(serviceAccountKey, projectID, zone string) error
}

type terraformExecutor interface {
	Apply(credentials, envID, projectID, zone, region, certPath, keyPath, domain, template, tfState string) (string, error)
	Destroy(serviceAccountKey, envID, projectID, zone, region, template, tfState string) (string, error)
	Version() (string, error)
}

type terraformOutputProvider interface {
	Get(tfState, lbType string) (terraform.Outputs, error)
}

type zones interface {
	Get(region string) []string
}

type boshManager interface {
	Create(storage.State, []byte) (storage.State, error)
	Delete(storage.State) error
	GetDeploymentVars(storage.State) (string, error)
}

type envIDManager interface {
	Sync(storage.State, string) (string, error)
}

func NewGCPUp(stateStore stateStore, keyPairUpdater keyPairUpdater, gcpProvider gcpProvider, terraformExecutor terraformExecutor,
	boshManager boshManager, logger logger, boshClientProvider boshClientProvider, cloudConfigGenerator gcpCloudConfigGenerator,
	terraformOutputProvider terraformOutputProvider, zones zones, envIDManager envIDManager) GCPUp {
	return GCPUp{
		stateStore:              stateStore,
		keyPairUpdater:          keyPairUpdater,
		gcpProvider:             gcpProvider,
		terraformExecutor:       terraformExecutor,
		boshManager:             boshManager,
		logger:                  logger,
		boshClientProvider:      boshClientProvider,
		cloudConfigGenerator:    cloudConfigGenerator,
		terraformOutputProvider: terraformOutputProvider,
		zones:        zones,
		envIDManager: envIDManager,
	}
}

func (u GCPUp) Execute(upConfig GCPUpConfig, state storage.State) error {
	err := fastFailTerraformVersion(u.terraformExecutor)
	if err != nil {
		return err
	}

	var opsFileContents []byte
	if !upConfig.empty() {
		var gcpDetails storage.GCP
		var err error
		gcpDetails, opsFileContents, err = u.parseUpConfig(upConfig)
		if err != nil {
			return err
		}

		state.IAAS = "gcp"

		if err := u.fastFailConflictingGCPState(gcpDetails, state.GCP); err != nil {
			return err
		}

		if upConfig.NoDirector {
			if !state.BOSH.IsEmpty() {
				return errors.New(`Director already exists, you must re-create your environment to use "--no-director"`)
			}

			state.NoDirector = true
		}

		state.GCP = gcpDetails
	}

	if err := u.validateState(state); err != nil {
		return err
	}

	if err := u.gcpProvider.SetConfig(state.GCP.ServiceAccountKey, state.GCP.ProjectID, state.GCP.Zone); err != nil {
		return err
	}

	envID, err := u.envIDManager.Sync(state, upConfig.Name)
	if err != nil {
		return err
	}

	state.EnvID = envID

	if err := u.stateStore.Set(state); err != nil {
		return err
	}

	if state.KeyPair.IsEmpty() {
		keyPair, err := u.keyPairUpdater.Update()
		if err != nil {
			return err
		}
		state.KeyPair = keyPair
		if err := u.stateStore.Set(state); err != nil {
			return err
		}
	}

	var template string
	zones := u.zones.Get(state.GCP.Region)
	switch state.LB.Type {
	case "concourse":
		template = strings.Join([]string{terraformVarsTemplate, terraformBOSHDirectorTemplate, terraformConcourseLBTemplate}, "\n")
	case "cf":
		terraformCFLBBackendService := generateBackendServiceTerraform(len(zones))
		instanceGroups := generateInstanceGroups(zones)
		template = strings.Join([]string{terraformVarsTemplate, terraformBOSHDirectorTemplate, terraformCFLBTemplate, instanceGroups, terraformCFLBBackendService}, "\n")
	default:
		template = strings.Join([]string{terraformVarsTemplate, terraformBOSHDirectorTemplate}, "\n")
	}

	tfState, err := u.terraformExecutor.Apply(state.GCP.ServiceAccountKey,
		state.EnvID, state.GCP.ProjectID, state.GCP.Zone, state.GCP.Region, state.LB.Cert, state.LB.Key, state.LB.Domain,
		template, state.TFState,
	)
	switch err.(type) {
	case terraform.TerraformApplyError:
		taErr := err.(terraform.TerraformApplyError)
		state.TFState = taErr.TFState()
		if setErr := u.stateStore.Set(state); setErr != nil {
			errorList := helpers.Errors{}
			errorList.Add(err)
			errorList.Add(setErr)
			return errorList
		}
		return err
	case error:
		return err
	}

	state.TFState = tfState
	if err := u.stateStore.Set(state); err != nil {
		return err
	}

	terraformOutputs, err := u.terraformOutputProvider.Get(state.TFState, state.LB.Type)
	if err != nil {
		return err
	}

	if !state.NoDirector {
		state, err = u.boshManager.Create(state, opsFileContents)
		switch err.(type) {
		case bosh.ManagerCreateError:
			bcErr := err.(bosh.ManagerCreateError)
			if setErr := u.stateStore.Set(bcErr.State()); setErr != nil {
				errorList := helpers.Errors{}
				errorList.Add(err)
				errorList.Add(setErr)
				return errorList
			}
			return err
		case error:
			return err
		}

		err = u.stateStore.Set(state)
		if err != nil {
			return err
		}

		boshClient := u.boshClientProvider.Client(state.BOSH.DirectorAddress, state.BOSH.DirectorUsername,
			state.BOSH.DirectorPassword)

		u.logger.Step("generating cloud config")
		cloudConfig, err := u.cloudConfigGenerator.Generate(gcpcloudconfig.CloudConfigInput{
			AZs:                 zones,
			Tags:                []string{terraformOutputs.InternalTag},
			NetworkName:         terraformOutputs.NetworkName,
			SubnetworkName:      terraformOutputs.SubnetworkName,
			ConcourseTargetPool: terraformOutputs.ConcourseTargetPool,
			CFBackends: gcpcloudconfig.CFBackends{
				Router:    terraformOutputs.RouterBackendService,
				SSHProxy:  terraformOutputs.SSHProxyTargetPool,
				TCPRouter: terraformOutputs.TCPRouterTargetPool,
				WS:        terraformOutputs.WSTargetPool,
			},
		})
		if err != nil {
			return err
		}

		manifestYAML, err := marshal(cloudConfig)
		if err != nil {
			return err // not tested
		}

		u.logger.Step("applying cloud config")
		if err := boshClient.UpdateCloudConfig(manifestYAML); err != nil {
			return err
		}

	}
	return nil
}

func (u GCPUp) validateState(state storage.State) error {
	switch {
	case state.GCP.ServiceAccountKey == "":
		return errors.New("GCP service account key must be provided")
	case state.GCP.ProjectID == "":
		return errors.New("GCP project ID must be provided")
	case state.GCP.Region == "":
		return errors.New("GCP region must be provided")
	case state.GCP.Zone == "":
		return errors.New("GCP zone must be provided")
	}

	return nil
}

func (u GCPUp) parseUpConfig(upConfig GCPUpConfig) (storage.GCP, []byte, error) {
	if upConfig.ServiceAccountKeyPath == "" {
		return storage.GCP{}, []byte{}, errors.New("GCP service account key must be provided")
	}

	sak, err := ioutil.ReadFile(upConfig.ServiceAccountKeyPath)
	if err != nil {
		return storage.GCP{}, []byte{}, fmt.Errorf("error reading service account key: %v", err)
	}

	var tmp interface{}
	err = json.Unmarshal(sak, &tmp)
	if err != nil {
		return storage.GCP{}, []byte{}, fmt.Errorf("error parsing service account key: %v", err)
	}

	var opsFileContents []byte
	if upConfig.OpsFilePath != "" {
		opsFileContents, err = ioutil.ReadFile(upConfig.OpsFilePath)
		if err != nil {
			return storage.GCP{}, []byte{}, fmt.Errorf("error reading ops-file contents: %v", err)
		}
	}

	return storage.GCP{
		ServiceAccountKey: string(sak),
		ProjectID:         upConfig.ProjectID,
		Zone:              upConfig.Zone,
		Region:            upConfig.Region,
	}, opsFileContents, nil
}

func (c GCPUpConfig) empty() bool {
	return c.ServiceAccountKeyPath == "" && c.ProjectID == "" && c.Region == "" && c.Zone == ""
}

func (u GCPUp) fastFailConflictingGCPState(configGCP storage.GCP, stateGCP storage.GCP) error {
	if stateGCP.Region != "" && stateGCP.Region != configGCP.Region {
		return errors.New(fmt.Sprintf("The region cannot be changed for an existing environment. The current region is %s.", stateGCP.Region))
	}

	if stateGCP.Zone != "" && stateGCP.Zone != configGCP.Zone {
		return errors.New(fmt.Sprintf("The zone cannot be changed for an existing environment. The current zone is %s.", stateGCP.Zone))
	}

	if stateGCP.ProjectID != "" && stateGCP.ProjectID != configGCP.ProjectID {
		return errors.New(fmt.Sprintf("The project id cannot be changed for an existing environment. The current project id is %s.", stateGCP.ProjectID))
	}

	return nil
}

func fastFailTerraformVersion(terraformExecutor terraformExecutor) error {
	type semver struct {
		major int
		minor int
		patch int
	}

	lessThan := func(s, other semver) bool {
		if s.major < other.major {
			return true
		}
		if s.major > other.major {
			return false
		}
		if s.minor < other.minor {
			return true
		}
		if s.minor > other.minor {
			return false
		}
		if s.patch < other.patch {
			return true
		}
		return false
	}

	minimumVersion := semver{
		major: 0,
		minor: 8,
		patch: 5,
	}

	version, err := terraformExecutor.Version()
	if err != nil {
		return err
	}
	semverParts := strings.Split(version, ".")
	majorVersion, err := strconv.Atoi(semverParts[0])
	if err != nil {
		return err
	}

	minorVersion, err := strconv.Atoi(semverParts[1])
	if err != nil {
		return err
	}

	patchVersion, err := strconv.Atoi(semverParts[2])
	if err != nil {
		return err
	}

	terraformVersion := semver{
		major: majorVersion,
		minor: minorVersion,
		patch: patchVersion,
	}

	if lessThan(terraformVersion, minimumVersion) {
		return errors.New("Terraform version must be at least v0.8.5")
	}

	return nil
}
