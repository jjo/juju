// Copyright 2012, 2013 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package testing

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/juju/errors"
	gitjujutesting "github.com/juju/testing"
	jc "github.com/juju/testing/checkers"
	"github.com/juju/utils"
	"github.com/juju/utils/arch"
	"github.com/juju/utils/series"
	"github.com/juju/utils/set"
	"github.com/juju/version"
	gc "gopkg.in/check.v1"
	"gopkg.in/juju/charm.v6-unstable"
	"gopkg.in/juju/charmrepo.v2-unstable"
	"gopkg.in/juju/names.v2"

	"github.com/juju/juju/agent"
	"github.com/juju/juju/api"
	"github.com/juju/juju/cert"
	"github.com/juju/juju/cloud"
	"github.com/juju/juju/cmd/modelcmd"
	"github.com/juju/juju/controller"
	"github.com/juju/juju/environs"
	"github.com/juju/juju/environs/bootstrap"
	"github.com/juju/juju/environs/config"
	"github.com/juju/juju/environs/filestorage"
	sstesting "github.com/juju/juju/environs/simplestreams/testing"
	"github.com/juju/juju/environs/storage"
	envtesting "github.com/juju/juju/environs/testing"
	"github.com/juju/juju/environs/tools"
	"github.com/juju/juju/juju"
	"github.com/juju/juju/juju/osenv"
	"github.com/juju/juju/jujuclient"
	"github.com/juju/juju/mongo"
	"github.com/juju/juju/mongo/mongotest"
	"github.com/juju/juju/provider/dummy"
	"github.com/juju/juju/state"
	"github.com/juju/juju/state/binarystorage"
	statestorage "github.com/juju/juju/state/storage"
	"github.com/juju/juju/testcharms"
	"github.com/juju/juju/testing"
	"github.com/juju/juju/testing/factory"
	jujuversion "github.com/juju/juju/version"
)

const ControllerName = "kontroll"

// JujuConnSuite provides a freshly bootstrapped juju.Conn
// for each test. It also includes testing.BaseSuite.
//
// It also sets up RootDir to point to a directory hierarchy
// mirroring the intended juju directory structure, including
// the following:
//     RootDir/var/lib/juju
//         An empty directory returned as DataDir - the
//         root of the juju data storage space.
// $HOME is set to point to RootDir/home/ubuntu.
type JujuConnSuite struct {
	// ConfigAttrs can be set up before SetUpTest
	// is invoked. Any attributes set here will be
	// added to the suite's environment configuration.
	ConfigAttrs map[string]interface{}

	// TODO: JujuConnSuite should not be concerned both with JUJU_DATA and with
	// /var/lib/juju: the use cases are completely non-overlapping, and any tests that
	// really do need both to exist ought to be embedding distinct fixtures for the
	// distinct environments.
	gitjujutesting.MgoSuite
	testing.FakeJujuXDGDataHomeSuite
	envtesting.ToolsFixture

	DefaultToolsStorageDir string
	DefaultToolsStorage    storage.Storage

	ControllerUUID     string
	State              *state.State
	Environ            environs.Environ
	APIState           api.Connection
	apiStates          []api.Connection // additional api.Connections to close on teardown
	ControllerStore    jujuclient.ClientStore
	BackingState       *state.State     // The State being used by the API server
	BackingStatePool   *state.StatePool // The StatePool being used by the API server
	RootDir            string           // The faked-up root directory.
	LogDir             string
	oldHome            string
	oldJujuXDGDataHome string
	DummyConfig        testing.Attrs
	Factory            *factory.Factory
}

const AdminSecret = "dummy-secret"

func (s *JujuConnSuite) SetUpSuite(c *gc.C) {
	s.MgoSuite.SetUpSuite(c)
	s.FakeJujuXDGDataHomeSuite.SetUpSuite(c)
	s.PatchValue(&utils.OutgoingAccessAllowed, false)
	s.PatchValue(&cert.KeyBits, 1024) // Use a shorter key for a faster TLS handshake.
}

func (s *JujuConnSuite) TearDownSuite(c *gc.C) {
	s.FakeJujuXDGDataHomeSuite.TearDownSuite(c)
	s.MgoSuite.TearDownSuite(c)
}

func (s *JujuConnSuite) SetUpTest(c *gc.C) {
	s.MgoSuite.SetUpTest(c)
	s.FakeJujuXDGDataHomeSuite.SetUpTest(c)
	s.ToolsFixture.SetUpTest(c)
	s.setUpConn(c)
	s.Factory = factory.NewFactory(s.State)
}

func (s *JujuConnSuite) TearDownTest(c *gc.C) {
	s.tearDownConn(c)
	s.ToolsFixture.TearDownTest(c)
	s.FakeJujuXDGDataHomeSuite.TearDownTest(c)
	s.MgoSuite.TearDownTest(c)
}

// Reset returns environment state to that which existed at the start of
// the test.
func (s *JujuConnSuite) Reset(c *gc.C) {
	s.tearDownConn(c)
	s.setUpConn(c)
}

func (s *JujuConnSuite) AdminUserTag(c *gc.C) names.UserTag {
	env, err := s.State.ControllerModel()
	c.Assert(err, jc.ErrorIsNil)
	return env.Owner()
}

func (s *JujuConnSuite) MongoInfo(c *gc.C) *mongo.MongoInfo {
	info := s.State.MongoConnectionInfo()
	info.Password = "dummy-secret"
	return info
}

func (s *JujuConnSuite) APIInfo(c *gc.C) *api.Info {
	controllerCfg := controller.ControllerConfig(s.Environ.Config().AllAttrs())
	apiInfo, err := environs.APIInfo(s.ControllerUUID, testing.ModelTag.Id(), testing.CACert, controllerCfg.APIPort(), s.Environ)
	c.Assert(err, jc.ErrorIsNil)
	apiInfo.Tag = s.AdminUserTag(c)
	apiInfo.Password = "dummy-secret"
	apiInfo.ModelTag = s.State.ModelTag()
	return apiInfo
}

// openAPIAs opens the API and ensures that the api.Connection returned will be
// closed during the test teardown by using a cleanup function.
func (s *JujuConnSuite) openAPIAs(c *gc.C, tag names.Tag, password, nonce string) api.Connection {
	apiInfo := s.APIInfo(c)
	apiInfo.Tag = tag
	apiInfo.Password = password
	apiInfo.Nonce = nonce
	apiState, err := api.Open(apiInfo, api.DialOpts{})
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(apiState, gc.NotNil)
	s.apiStates = append(s.apiStates, apiState)
	return apiState
}

// OpenAPIAs opens the API using the given identity tag and password for
// authentication.  The returned api.Connection should not be closed by the caller
// as a cleanup function has been registered to do that.
func (s *JujuConnSuite) OpenAPIAs(c *gc.C, tag names.Tag, password string) api.Connection {
	return s.openAPIAs(c, tag, password, "")
}

// OpenAPIAsMachine opens the API using the given machine tag, password and
// nonce for authentication. The returned api.Connection should not be closed by
// the caller as a cleanup function has been registered to do that.
func (s *JujuConnSuite) OpenAPIAsMachine(c *gc.C, tag names.Tag, password, nonce string) api.Connection {
	return s.openAPIAs(c, tag, password, nonce)
}

// OpenAPIAsNewMachine creates a new machine entry that lives in system state,
// and then uses that to open the API. The returned api.Connection should not be
// closed by the caller as a cleanup function has been registered to do that.
// The machine will run the supplied jobs; if none are given, JobHostUnits is assumed.
func (s *JujuConnSuite) OpenAPIAsNewMachine(c *gc.C, jobs ...state.MachineJob) (api.Connection, *state.Machine) {
	if len(jobs) == 0 {
		jobs = []state.MachineJob{state.JobHostUnits}
	}
	machine, err := s.State.AddMachine("quantal", jobs...)
	c.Assert(err, jc.ErrorIsNil)
	password, err := utils.RandomPassword()
	c.Assert(err, jc.ErrorIsNil)
	err = machine.SetPassword(password)
	c.Assert(err, jc.ErrorIsNil)
	err = machine.SetProvisioned("foo", "fake_nonce", nil)
	c.Assert(err, jc.ErrorIsNil)
	return s.openAPIAs(c, machine.Tag(), password, "fake_nonce"), machine
}

// DefaultVersions returns a slice of unique 'versions' for the current
// environment's preferred series and host architecture, as well supported LTS
// series for the host architecture. Additionally, it ensures that 'versions'
// for amd64 are returned if that is not the current host's architecture.
func DefaultVersions(conf *config.Config) []version.Binary {
	var versions []version.Binary
	supported := series.SupportedLts()
	defaultSeries := set.NewStrings(supported...)
	defaultSeries.Add(config.PreferredSeries(conf))
	defaultSeries.Add(series.HostSeries())
	agentVersion, set := conf.AgentVersion()
	if !set {
		agentVersion = jujuversion.Current
	}
	for _, s := range defaultSeries.Values() {
		versions = append(versions, version.Binary{
			Number: agentVersion,
			Arch:   arch.HostArch(),
			Series: s,
		})
		if arch.HostArch() != "amd64" {
			versions = append(versions, version.Binary{
				Number: agentVersion,
				Arch:   "amd64",
				Series: s,
			})

		}
	}
	return versions
}

func (s *JujuConnSuite) setUpConn(c *gc.C) {
	if s.RootDir != "" {
		c.Fatal("JujuConnSuite.setUpConn without teardown")
	}
	s.RootDir = c.MkDir()
	s.oldHome = utils.Home()
	home := filepath.Join(s.RootDir, "/home/ubuntu")
	err := os.MkdirAll(home, 0777)
	c.Assert(err, jc.ErrorIsNil)
	err = utils.SetHome(home)
	c.Assert(err, jc.ErrorIsNil)

	err = os.MkdirAll(filepath.Join(home, ".local", "share"), 0777)
	c.Assert(err, jc.ErrorIsNil)

	s.oldJujuXDGDataHome = osenv.SetJujuXDGDataHome(filepath.Join(home, ".local", "share", "juju"))
	err = os.MkdirAll(osenv.JujuXDGDataHome(), 0777)
	c.Assert(err, jc.ErrorIsNil)

	err = os.MkdirAll(s.DataDir(), 0777)
	c.Assert(err, jc.ErrorIsNil)
	s.PatchEnvironment(osenv.JujuModelEnvKey, "controller")

	cfg, err := config.New(config.UseDefaults, (map[string]interface{})(s.sampleConfig()))
	c.Assert(err, jc.ErrorIsNil)

	s.ControllerStore = jujuclient.NewFileClientStore()

	ctx := testing.Context(c)
	environ, err := environs.Prepare(
		modelcmd.BootstrapContext(ctx),
		s.ControllerStore,
		environs.PrepareParams{
			BaseConfig:     cfg.AllAttrs(),
			Credential:     cloud.NewEmptyCredential(),
			ControllerName: ControllerName,
			CloudName:      "dummy",
		},
	)
	c.Assert(err, jc.ErrorIsNil)
	// sanity check we've got the correct environment.
	c.Assert(environ.Config().Name(), gc.Equals, "controller")
	s.PatchValue(&dummy.DataDir, s.DataDir())
	s.LogDir = c.MkDir()
	s.PatchValue(&dummy.LogDir, s.LogDir)

	versions := DefaultVersions(environ.Config())

	// Upload tools for both preferred and fake default series
	s.DefaultToolsStorageDir = c.MkDir()
	s.PatchValue(&tools.DefaultBaseURL, s.DefaultToolsStorageDir)
	stor, err := filestorage.NewFileStorageWriter(s.DefaultToolsStorageDir)
	c.Assert(err, jc.ErrorIsNil)
	// Upload tools to both release and devel streams since config will dictate that we
	// end up looking in both places.
	envtesting.AssertUploadFakeToolsVersions(c, stor, "released", "released", versions...)
	envtesting.AssertUploadFakeToolsVersions(c, stor, "devel", "devel", versions...)
	s.DefaultToolsStorage = stor

	s.PatchValue(&juju.JujuPublicKey, sstesting.SignedMetadataPublicKey)
	s.ControllerUUID = testing.ModelTag.Id()
	err = bootstrap.Bootstrap(modelcmd.BootstrapContext(ctx), environ, bootstrap.BootstrapParams{
		ControllerUUID: s.ControllerUUID,
		CloudName:      "dummy",
		Cloud: cloud.Cloud{
			Type:      "dummy",
			AuthTypes: []cloud.AuthType{cloud.EmptyAuthType},
		},
	})
	c.Assert(err, jc.ErrorIsNil)

	getStater := environ.(GetStater)
	s.BackingState = getStater.GetStateInAPIServer()
	s.BackingStatePool = getStater.GetStatePoolInAPIServer()

	s.State, err = newState(environ, s.BackingState.MongoConnectionInfo())
	c.Assert(err, jc.ErrorIsNil)

	controllerCfg := controller.ControllerConfig(environ.Config().AllAttrs())
	apiInfo, err := environs.APIInfo(s.ControllerUUID, testing.ModelTag.Id(), testing.CACert, controllerCfg.APIPort(), environ)
	c.Assert(err, jc.ErrorIsNil)
	apiInfo.Tag = s.AdminUserTag(c)
	apiInfo.Password = environ.Config().AdminSecret()
	s.APIState, err = api.Open(apiInfo, api.DialOpts{})
	c.Assert(err, jc.ErrorIsNil)

	err = s.State.SetAPIHostPorts(s.APIState.APIHostPorts())
	c.Assert(err, jc.ErrorIsNil)

	// Make sure the controller store has the controller api endpoint address set
	controller, err := s.ControllerStore.ControllerByName(ControllerName)
	c.Assert(err, jc.ErrorIsNil)
	controller.APIEndpoints = []string{s.APIState.APIHostPorts()[0][0].String()}
	err = s.ControllerStore.UpdateController(ControllerName, *controller)
	c.Assert(err, jc.ErrorIsNil)
	err = s.ControllerStore.SetCurrentController(ControllerName)
	c.Assert(err, jc.ErrorIsNil)

	s.Environ = environ

	// Insert expected values...
	servingInfo := state.StateServingInfo{
		PrivateKey:   testing.ServerKey,
		Cert:         testing.ServerCert,
		CAPrivateKey: testing.CAKey,
		SharedSecret: "really, really secret",
		APIPort:      controllerCfg.APIPort(),
		StatePort:    controllerCfg.StatePort(),
	}
	s.State.SetStateServingInfo(servingInfo)
}

// AddToolsToState adds tools to tools storage.
func (s *JujuConnSuite) AddToolsToState(c *gc.C, versions ...version.Binary) {
	stor, err := s.State.ToolsStorage()
	c.Assert(err, jc.ErrorIsNil)
	defer stor.Close()
	for _, v := range versions {
		content := v.String()
		hash := fmt.Sprintf("sha256(%s)", content)
		err := stor.Add(strings.NewReader(content), binarystorage.Metadata{
			Version: v.String(),
			Size:    int64(len(content)),
			SHA256:  hash,
		})
		c.Assert(err, jc.ErrorIsNil)
	}
}

// AddDefaultTools adds tools to tools storage for default juju
// series and architectures.
func (s *JujuConnSuite) AddDefaultToolsToState(c *gc.C) {
	versions := DefaultVersions(s.Environ.Config())
	s.AddToolsToState(c, versions...)
}

var redialStrategy = utils.AttemptStrategy{
	Total: 60 * time.Second,
	Delay: 250 * time.Millisecond,
}

// newState returns a new State that uses the given environment.
// The environment must have already been bootstrapped.
func newState(environ environs.Environ, mongoInfo *mongo.MongoInfo) (*state.State, error) {
	config := environ.Config()
	password := config.AdminSecret()
	if password == "" {
		return nil, fmt.Errorf("cannot connect without admin-secret")
	}
	modelTag := names.NewModelTag(config.UUID())

	mongoInfo.Password = password
	opts := mongotest.DialOpts()
	st, err := state.Open(modelTag, mongoInfo, opts, environs.NewStatePolicy())
	if errors.IsUnauthorized(errors.Cause(err)) {
		// We try for a while because we might succeed in
		// connecting to mongo before the state has been
		// initialized and the initial password set.
		for a := redialStrategy.Start(); a.Next(); {
			st, err = state.Open(modelTag, mongoInfo, opts, environs.NewStatePolicy())
			if !errors.IsUnauthorized(errors.Cause(err)) {
				break
			}
		}
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if err := updateSecrets(environ, st); err != nil {
		st.Close()
		return nil, fmt.Errorf("unable to push secrets: %v", err)
	}
	return st, nil
}

func updateSecrets(env environs.Environ, st *state.State) error {
	secrets, err := env.Provider().SecretAttrs(env.Config())
	if err != nil {
		return err
	}
	cfg, err := st.ModelConfig()
	if err != nil {
		return err
	}
	secretAttrs := make(map[string]interface{})
	attrs := cfg.AllAttrs()
	for k, v := range secrets {
		if _, exists := attrs[k]; exists {
			// Environment already has secrets. Won't send again.
			return nil
		} else {
			secretAttrs[k] = v
		}
	}
	return st.UpdateModelConfig(secretAttrs, nil, nil)
}

// PutCharm uploads the given charm to provider storage, and adds a
// state.Charm to the state.  The charm is not uploaded if a charm with
// the same URL already exists in the state.
// If bumpRevision is true, the charm must be a local directory,
// and the revision number will be incremented before pushing.
func PutCharm(st *state.State, curl *charm.URL, repo charmrepo.Interface, bumpRevision bool) (*state.Charm, error) {
	if curl.Revision == -1 {
		var err error
		curl, _, err = repo.Resolve(curl)
		if err != nil {
			return nil, fmt.Errorf("cannot get latest charm revision: %v", err)
		}
	}
	ch, err := repo.Get(curl)
	if err != nil {
		return nil, fmt.Errorf("cannot get charm: %v", err)
	}
	if bumpRevision {
		chd, ok := ch.(*charm.CharmDir)
		if !ok {
			return nil, fmt.Errorf("cannot increment revision of charm %q: not a directory", curl)
		}
		if err = chd.SetDiskRevision(chd.Revision() + 1); err != nil {
			return nil, fmt.Errorf("cannot increment revision of charm %q: %v", curl, err)
		}
		curl = curl.WithRevision(chd.Revision())
	}
	if sch, err := st.Charm(curl); err == nil {
		return sch, nil
	}
	return AddCharm(st, curl, ch)
}

// AddCharm adds the charm to state and storage.
func AddCharm(st *state.State, curl *charm.URL, ch charm.Charm) (*state.Charm, error) {
	var f *os.File
	name := charm.Quote(curl.String())
	switch ch := ch.(type) {
	case *charm.CharmDir:
		var err error
		if f, err = ioutil.TempFile("", name); err != nil {
			return nil, err
		}
		defer os.Remove(f.Name())
		defer f.Close()
		err = ch.ArchiveTo(f)
		if err != nil {
			return nil, fmt.Errorf("cannot bundle charm: %v", err)
		}
		if _, err := f.Seek(0, 0); err != nil {
			return nil, err
		}
	case *charm.CharmArchive:
		var err error
		if f, err = os.Open(ch.Path); err != nil {
			return nil, fmt.Errorf("cannot read charm bundle: %v", err)
		}
		defer f.Close()
	default:
		return nil, fmt.Errorf("unknown charm type %T", ch)
	}
	digest, size, err := utils.ReadSHA256(f)
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}

	stor := statestorage.NewStorage(st.ModelUUID(), st.MongoSession())
	storagePath := fmt.Sprintf("/charms/%s-%s", curl.String(), digest)
	if err := stor.Put(storagePath, f, size); err != nil {
		return nil, fmt.Errorf("cannot put charm: %v", err)
	}
	info := state.CharmInfo{
		Charm:       ch,
		ID:          curl,
		StoragePath: storagePath,
		SHA256:      digest,
	}
	sch, err := st.AddCharm(info)
	if err != nil {
		return nil, fmt.Errorf("cannot add charm: %v", err)
	}
	return sch, nil
}

func (s *JujuConnSuite) sampleConfig() testing.Attrs {
	if s.DummyConfig == nil {
		s.DummyConfig = dummy.SampleConfig()
	}
	attrs := s.DummyConfig.Merge(testing.Attrs{
		"name":           "controller",
		"admin-secret":   AdminSecret,
		"agent-version":  jujuversion.Current.String(),
		"ca-cert":        testing.CACert,
		"ca-private-key": testing.CAKey,
	})
	// Add any custom attributes required.
	for attr, val := range s.ConfigAttrs {
		attrs[attr] = val
	}
	return attrs
}

type GetStater interface {
	GetStateInAPIServer() *state.State
	GetStatePoolInAPIServer() *state.StatePool
}

func (s *JujuConnSuite) tearDownConn(c *gc.C) {
	testServer := gitjujutesting.MgoServer.Addr()
	serverAlive := testServer != ""

	// Close any api connections we know about first.
	for _, st := range s.apiStates {
		err := st.Close()
		if serverAlive {
			c.Check(err, jc.ErrorIsNil)
		}
	}
	s.apiStates = nil
	if s.APIState != nil {
		err := s.APIState.Close()
		s.APIState = nil
		if serverAlive {
			c.Check(err, gc.IsNil,
				gc.Commentf("closing api state failed\n%s\n", errors.ErrorStack(err)),
			)
		}
	}
	// Close state.
	if s.State != nil {
		err := s.State.Close()
		if serverAlive {
			// This happens way too often with failing tests,
			// so add some context in case of an error.
			c.Check(err, gc.IsNil,
				gc.Commentf("closing state failed\n%s\n", errors.ErrorStack(err)),
			)
		}
		s.State = nil
	}

	dummy.Reset(c)
	err := utils.SetHome(s.oldHome)
	c.Assert(err, jc.ErrorIsNil)
	osenv.SetJujuXDGDataHome(s.oldJujuXDGDataHome)
	s.oldHome = ""
	s.RootDir = ""
}

func (s *JujuConnSuite) DataDir() string {
	if s.RootDir == "" {
		panic("DataDir called out of test context")
	}
	return filepath.Join(s.RootDir, "/var/lib/juju")
}

func (s *JujuConnSuite) ConfDir() string {
	if s.RootDir == "" {
		panic("DataDir called out of test context")
	}
	return filepath.Join(s.RootDir, "/etc/juju")
}

func (s *JujuConnSuite) AddTestingCharm(c *gc.C, name string) *state.Charm {
	ch := testcharms.Repo.CharmDir(name)
	ident := fmt.Sprintf("%s-%d", ch.Meta().Name, ch.Revision())
	curl := charm.MustParseURL("local:quantal/" + ident)
	repo, err := charmrepo.InferRepository(
		curl,
		charmrepo.NewCharmStoreParams{},
		testcharms.Repo.Path())
	c.Assert(err, jc.ErrorIsNil)
	sch, err := PutCharm(s.State, curl, repo, false)
	c.Assert(err, jc.ErrorIsNil)
	return sch
}

func (s *JujuConnSuite) AddTestingService(c *gc.C, name string, ch *state.Charm) *state.Application {
	app, err := s.State.AddApplication(state.AddApplicationArgs{Name: name, Charm: ch})
	c.Assert(err, jc.ErrorIsNil)
	return app

}

func (s *JujuConnSuite) AddTestingServiceWithStorage(c *gc.C, name string, ch *state.Charm, storage map[string]state.StorageConstraints) *state.Application {
	app, err := s.State.AddApplication(state.AddApplicationArgs{Name: name, Charm: ch, Storage: storage})
	c.Assert(err, jc.ErrorIsNil)
	return app
}

func (s *JujuConnSuite) AddTestingServiceWithBindings(c *gc.C, name string, ch *state.Charm, bindings map[string]string) *state.Application {
	app, err := s.State.AddApplication(state.AddApplicationArgs{Name: name, Charm: ch, EndpointBindings: bindings})
	c.Assert(err, jc.ErrorIsNil)
	return app
}

func (s *JujuConnSuite) AgentConfigForTag(c *gc.C, tag names.Tag) agent.ConfigSetter {
	password, err := utils.RandomPassword()
	c.Assert(err, jc.ErrorIsNil)
	paths := agent.DefaultPaths
	paths.DataDir = s.DataDir()
	config, err := agent.NewAgentConfig(
		agent.AgentConfigParams{
			Paths:             paths,
			Tag:               tag,
			UpgradedToVersion: jujuversion.Current,
			Password:          password,
			Nonce:             "nonce",
			StateAddresses:    s.MongoInfo(c).Addrs,
			APIAddresses:      s.APIInfo(c).Addrs,
			CACert:            testing.CACert,
			Model:             s.State.ModelTag(),
		})
	c.Assert(err, jc.ErrorIsNil)
	return config
}

// AssertConfigParameterUpdated updates environment parameter and
// asserts that no errors were encountered
func (s *JujuConnSuite) AssertConfigParameterUpdated(c *gc.C, key string, value interface{}) {
	err := s.BackingState.UpdateModelConfig(map[string]interface{}{key: value}, nil, nil)
	c.Assert(err, jc.ErrorIsNil)
}
