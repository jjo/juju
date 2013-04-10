package apiserver_test

import (
	"errors"
	"fmt"
	"io"
	. "launchpad.net/gocheck"
	"launchpad.net/juju-core/charm"
	"launchpad.net/juju-core/constraints"
	"launchpad.net/juju-core/juju/testing"
	"launchpad.net/juju-core/rpc"
	"launchpad.net/juju-core/state"
	"launchpad.net/juju-core/state/api"
	"launchpad.net/juju-core/state/api/params"
	"launchpad.net/juju-core/state/apiserver"
	coretesting "launchpad.net/juju-core/testing"
	"net"
	"strings"
	stdtesting "testing"
	"time"
)

func TestAll(t *stdtesting.T) {
	coretesting.MgoTestPackage(t)
}

type suite struct {
	testing.JujuConnSuite
	listener net.Listener
}

var _ = Suite(&suite{})

func removeServiceAndUnits(c *C, service *state.Service) {
	// Destroy all units for the service.
	units, err := service.AllUnits()
	c.Assert(err, IsNil)
	for _, unit := range units {
		err = unit.EnsureDead()
		c.Assert(err, IsNil)
		err = unit.Remove()
		c.Assert(err, IsNil)
	}
	// TODO: Calling Refresh is required due to LP bug #1152717 - remove when fixed.
	err = service.Refresh()
	c.Assert(err, IsNil)
	err = service.Destroy()
	c.Assert(err, IsNil)

	err = service.Refresh()
	c.Assert(state.IsNotFound(err), Equals, true)
}

var operationPermTests = []struct {
	about string
	// op performs the operation to be tested using the given state
	// connection.  It returns a function that should be used to
	// undo any changes made by the operation.
	op    func(c *C, st *api.State, mst *state.State) (reset func(), err error)
	allow []string
	deny  []string
}{{
	about: "Unit.Get",
	op:    opGetUnitWordpress0,
	deny:  []string{"user-admin", "user-other"},
}, {
	about: "Machine.Get",
	op:    opGetMachine1,
	deny:  []string{"user-admin", "user-other"},
}, {
	about: "Machine.SetPassword",
	op:    opMachine1SetPassword,
	allow: []string{"machine-0", "machine-1"},
}, {
	about: "Unit.SetPassword (on principal unit)",
	op:    opUnitSetPassword("wordpress/0"),
	allow: []string{"unit-wordpress-0", "machine-1"},
}, {
	about: "Unit.SetPassword (on subordinate unit)",
	op:    opUnitSetPassword("logging/0"),
	allow: []string{"unit-logging-0", "unit-wordpress-0"},
}, {
	about: "Client.Status",
	op:    opClientStatus,
	allow: []string{"user-admin", "user-other"},
}, {
	about: "Client.ServiceSet",
	op:    opClientServiceSet,
	allow: []string{"user-admin", "user-other"},
}, {
	about: "Client.ServiceSetYAML",
	op:    opClientServiceSetYAML,
	allow: []string{"user-admin", "user-other"},
}, {
	about: "Client.ServiceGet",
	op:    opClientServiceGet,
	allow: []string{"user-admin", "user-other"},
}, {
	about: "Client.Resolved",
	op:    opClientResolved,
	allow: []string{"user-admin", "user-other"},
}, {
	about: "Client.ServiceExpose",
	op:    opClientServiceExpose,
	allow: []string{"user-admin", "user-other"},
}, {
	about: "Client.ServiceUnexpose",
	op:    opClientServiceUnexpose,
	allow: []string{"user-admin", "user-other"},
}, {
	about: "Client.ServiceDeploy",
	op:    opClientServiceDeploy,
	allow: []string{"user-admin", "user-other"},
}, {
	about: "Client.GetAnnotations",
	op:    opClientGetAnnotations,
	allow: []string{"user-admin", "user-other"},
}, {
	about: "Client.SetAnnotations",
	op:    opClientSetAnnotations,
	allow: []string{"user-admin", "user-other"},
}, {
	about: "Client.AddServiceUnits",
	op:    opClientAddServiceUnits,
	allow: []string{"user-admin", "user-other"},
}, {
	about: "Client.DestroyServiceUnits",
	op:    opClientDestroyServiceUnits,
	allow: []string{"user-admin", "user-other"},
}, {
	about: "Client.ServiceDestroy",
	op:    opClientServiceDestroy,
	allow: []string{"user-admin", "user-other"},
}, {
	about: "Client.GetServiceConstraints",
	op:    opClientGetServiceConstraints,
	allow: []string{"user-admin", "user-other"},
}, {
	about: "Client.SetServiceConstraints",
	op:    opClientSetServiceConstraints,
	allow: []string{"user-admin", "user-other"},
}, {
	about: "Client.WatchAll",
	op:    opClientWatchAll,
	allow: []string{"user-admin", "user-other"},
}, {
	about: "Client.CharmInfo",
	op:    opClientCharmInfo,
	allow: []string{"user-admin", "user-other"},
}, {
	about: "Client.AddRelation",
	op:    opClientAddRelation,
	allow: []string{"user-admin", "user-other"},
}, {
	about: "Client.DestroyRelation",
	op:    opClientDestroyRelation,
	allow: []string{"user-admin", "user-other"},
},
}

// allowed returns the set of allowed entities given an allow list and a
// deny list.  If an allow list is specified, only those entities are
// allowed; otherwise those in deny are disallowed.
func allowed(all, allow, deny []string) map[string]bool {
	p := make(map[string]bool)
	if allow != nil {
		for _, e := range allow {
			p[e] = true
		}
		return p
	}
loop:
	for _, e0 := range all {
		for _, e1 := range deny {
			if e1 == e0 {
				continue loop
			}
		}
		p[e0] = true
	}
	return p
}

func (s *suite) TestOperationPerm(c *C) {
	entities := s.setUpScenario(c)
	for i, t := range operationPermTests {
		allow := allowed(entities, t.allow, t.deny)
		for _, e := range entities {
			c.Logf("test %d; %s; entity %q", i, t.about, e)
			st := s.openAs(c, e)
			reset, err := t.op(c, st, s.State)
			if allow[e] {
				c.Check(err, IsNil)
			} else {
				c.Check(err, ErrorMatches, "permission denied")
				c.Check(api.ErrCode(err), Equals, api.CodeUnauthorized)
			}
			reset()
			st.Close()
		}
	}
}

func opGetUnitWordpress0(c *C, st *api.State, mst *state.State) (func(), error) {
	u, err := st.Unit("wordpress/0")
	if err != nil {
		c.Check(u, IsNil)
	} else {
		name, ok := u.DeployerTag()
		c.Check(ok, Equals, true)
		c.Check(name, Equals, "machine-1")
	}
	return func() {}, err
}

func opUnitSetPassword(unitName string) func(c *C, st *api.State, mst *state.State) (func(), error) {
	return func(c *C, st *api.State, mst *state.State) (func(), error) {
		u, err := st.Unit(unitName)
		if err != nil {
			c.Check(u, IsNil)
			return func() {}, err
		}
		err = u.SetPassword("another password")
		if err != nil {
			return func() {}, err
		}
		return func() {
			setDefaultPassword(c, u)
		}, nil
	}
}

func opGetMachine1(c *C, st *api.State, mst *state.State) (func(), error) {
	m, err := st.Machine("1")
	if err != nil {
		c.Check(m, IsNil)
	} else {
		name, ok := m.InstanceId()
		c.Assert(ok, Equals, true)
		c.Assert(name, Equals, "i-machine-1")
	}
	return func() {}, err
}

func opMachine1SetPassword(c *C, st *api.State, mst *state.State) (func(), error) {
	m, err := st.Machine("1")
	if err != nil {
		c.Check(m, IsNil)
		return func() {}, err
	}
	err = m.SetPassword("another password")
	if err != nil {
		return func() {}, err
	}
	return func() {
		setDefaultPassword(c, m)
	}, nil
}

func opClientCharmInfo(c *C, st *api.State, mst *state.State) (func(), error) {
	info, err := st.Client().CharmInfo("local:series/wordpress-3")
	if err != nil {
		c.Check(info, IsNil)
		return func() {}, err
	}
	c.Assert(info.URL, Equals, "local:series/wordpress-3")
	c.Assert(info.Meta.Name, Equals, "wordpress")
	c.Assert(info.Revision, Equals, 3)
	return func() {}, nil
}

func opClientAddRelation(c *C, st *api.State, mst *state.State) (func(), error) {
	_, err := st.Client().AddRelation("nosuch1", "nosuch2")
	if api.ErrCode(err) == api.CodeNotFound {
		err = nil
	}
	return func() {}, err
}

func opClientDestroyRelation(c *C, st *api.State, mst *state.State) (func(), error) {
	err := st.Client().DestroyRelation("nosuch1", "nosuch2")
	if api.ErrCode(err) == api.CodeNotFound {
		err = nil
	}
	return func() {}, err
}

func opClientStatus(c *C, st *api.State, mst *state.State) (func(), error) {
	status, err := st.Client().Status()
	if err != nil {
		c.Check(status, IsNil)
		return func() {}, err
	}
	c.Assert(status, DeepEquals, scenarioStatus)
	return func() {}, nil
}

func resetBlogTitle(c *C, st *api.State) func() {
	return func() {
		err := st.Client().ServiceSet("wordpress", map[string]string{
			"blog-title": "",
		})
		c.Assert(err, IsNil)
	}
}

func opClientServiceSet(c *C, st *api.State, mst *state.State) (func(), error) {
	err := st.Client().ServiceSet("wordpress", map[string]string{
		"blog-title": "foo",
	})
	if err != nil {
		return func() {}, err
	}
	return resetBlogTitle(c, st), nil
}

func opClientServiceSetYAML(c *C, st *api.State, mst *state.State) (func(), error) {
	err := st.Client().ServiceSetYAML("wordpress", `"blog-title": "foo"`)
	if err != nil {
		return func() {}, err
	}
	return resetBlogTitle(c, st), nil
}

func opClientServiceGet(c *C, st *api.State, mst *state.State) (func(), error) {
	// This test only shows that the call is made without error, ensuring the
	// signatures match.
	_, err := st.Client().ServiceGet("wordpress")
	if err != nil {
		return func() {}, err
	}
	return func() {}, nil
}

func opClientServiceExpose(c *C, st *api.State, mst *state.State) (func(), error) {
	// This test only shows that the call is made without error, ensuring the
	// signatures match.
	err := st.Client().ServiceExpose("wordpress")
	if err != nil {
		return func() {}, err
	}
	return func() {
		svc, err := mst.Service("wordpress")
		c.Assert(err, IsNil)
		svc.ClearExposed()
	}, nil
}

func opClientServiceUnexpose(c *C, st *api.State, mst *state.State) (func(), error) {
	// This test only checks that the call is made without error, ensuring the
	// signatures match.
	err := st.Client().ServiceUnexpose("wordpress")
	if err != nil {
		return func() {}, err
	}
	return func() {}, nil
}

func opClientResolved(c *C, st *api.State, _ *state.State) (func(), error) {
	err := st.Client().Resolved("wordpress/0", false)
	// There are several scenarios in which this test is called, one is
	// that the user is not authorized.  In that case we want to exit now,
	// letting the error percolate out so the caller knows that the
	// permission error was correctly generated.
	if err != nil && api.ErrCode(err) == api.CodeUnauthorized {
		return func() {}, err
	}
	// Otherwise, the user was authorized, but we expect an error anyway
	// because the unit is not in an error state when we tried to resolve
	// the error.  Therefore, since it is complaining it means that the
	// call to Resolved worked, so we're happy.
	c.Assert(err, NotNil)
	c.Assert(err.Error(), Equals, `unit "wordpress/0" is not in an error state`)
	return func() {}, nil
}

func opClientGetAnnotations(c *C, st *api.State, mst *state.State) (func(), error) {
	ann, err := st.Client().GetAnnotations("service-wordpress")
	if err != nil {
		return func() {}, err
	}
	c.Assert(ann, DeepEquals, make(map[string]string))
	return func() {}, nil
}

func opClientSetAnnotations(c *C, st *api.State, mst *state.State) (func(), error) {
	pairs := map[string]string{"key1": "value1", "key2": "value2"}
	err := st.Client().SetAnnotations("service-wordpress", pairs)
	if err != nil {
		return func() {}, err
	}
	return func() {
		pairs := map[string]string{"key1": "", "key2": ""}
		st.Client().SetAnnotations("service-wordpress", pairs)
	}, nil
}

func opClientServiceDeploy(c *C, st *api.State, mst *state.State) (func(), error) {
	// This test only checks that the call is made without error, ensuring the
	// signatures match.
	// We are cheating and using a local repo only.

	// Set the CharmStore to the test repository.
	serviceName := "mywordpress"
	charmUrl := "local:series/wordpress"
	parsedUrl := charm.MustParseURL(charmUrl)
	repo, err := charm.InferRepository(parsedUrl, coretesting.Charms.Path)
	originalServerCharmStore := apiserver.CharmStore
	apiserver.CharmStore = repo

	err = st.Client().ServiceDeploy(charmUrl, serviceName, 1, "", constraints.Value{})
	if err != nil {
		return func() {}, err
	}
	return func() {
		apiserver.CharmStore = originalServerCharmStore
		service, err := mst.Service(serviceName)
		c.Assert(err, IsNil)
		removeServiceAndUnits(c, service)
	}, nil
}

func opClientAddServiceUnits(c *C, st *api.State, mst *state.State) (func(), error) {
	err := st.Client().AddServiceUnits("nosuch", 1)
	if api.ErrCode(err) == api.CodeNotFound {
		err = nil
	}
	return func() {}, err
}

func opClientDestroyServiceUnits(c *C, st *api.State, mst *state.State) (func(), error) {
	err := st.Client().DestroyServiceUnits([]string{"wordpress/99"})
	if err != nil && strings.HasPrefix(err.Error(), "no units were destroyed") {
		err = nil
	}
	return func() {}, err
}

func opClientServiceDestroy(c *C, st *api.State, mst *state.State) (func(), error) {
	// This test only checks that the call is made without error, ensuring the
	// signatures match.
	err := st.Client().ServiceDestroy("non-existent")
	if api.ErrCode(err) == api.CodeNotFound {
		err = nil
	}
	return func() {}, err
}

func opClientGetServiceConstraints(c *C, st *api.State, mst *state.State) (func(), error) {
	// This test only checks that the call is made without error, ensuring the
	// signatures match.
	_, err := st.Client().GetServiceConstraints("wordpress")
	return func() {}, err
}

func opClientSetServiceConstraints(c *C, st *api.State, mst *state.State) (func(), error) {
	// This test only checks that the call is made without error, ensuring the
	// signatures match.
	nullConstraints := constraints.Value{}
	err := st.Client().SetServiceConstraints("wordpress", nullConstraints)
	if err != nil {
		return func() {}, err
	}
	return func() {}, nil
}

func opClientWatchAll(c *C, st *api.State, mst *state.State) (func(), error) {
	watcher, err := st.Client().WatchAll()
	if err == nil {
		watcher.Stop()
	}
	return func() {}, err
}

// scenarioStatus describes the expected state
// of the juju environment set up by setUpScenario.
var scenarioStatus = &api.Status{
	Machines: map[string]api.MachineInfo{
		"0": {
			InstanceId: "i-machine-0",
		},
		"1": {
			InstanceId: "i-machine-1",
		},
		"2": {
			InstanceId: "i-machine-2",
		},
	},
}

// setUpScenario makes an environment scenario suitable for
// testing most kinds of access scenario. It returns
// a list of all the entities in the scenario.
//
// When the scenario is initialized, we have:
// user-admin
// user-other
// machine-0
//  instance-id="i-machine-0"
//  nonce="fake_nonce"
//  jobs=manage-environ
// machine-1
//  instance-id="i-machine-1"
//  nonce="fake_nonce"
//  jobs=host-units
// machine-2
//  instance-id="i-machine-2"
//  nonce="fake_nonce"
//  jobs=host-units
// service-wordpress
// service-logging
// unit-wordpress-0
//     deployer-name=machine-1
// unit-logging-0
//  deployer-name=unit-wordpress-0
// unit-wordpress-1
//     deployer-name=machine-2
// unit-logging-1
//  deployer-name=unit-wordpress-1
//
// The passwords for all returned entities are
// set to the entity name with a " password" suffix.
//
// Note that there is nothing special about machine-0
// here - it's the environment manager in this scenario
// just because machine 0 has traditionally been the
// environment manager (bootstrap machine), so is
// hopefully easier to remember as such.
func (s *suite) setUpScenario(c *C) (entities []string) {
	add := func(e state.Tagger) {
		entities = append(entities, e.Tag())
	}
	u, err := s.State.User("admin")
	c.Assert(err, IsNil)
	setDefaultPassword(c, u)
	add(u)

	u, err = s.State.AddUser("other", "")
	c.Assert(err, IsNil)
	setDefaultPassword(c, u)
	add(u)

	m, err := s.State.AddMachine("series", state.JobManageEnviron)
	c.Assert(err, IsNil)
	c.Assert(m.Tag(), Equals, "machine-0")
	err = m.SetProvisioned(state.InstanceId("i-"+m.Tag()), "fake_nonce")
	c.Assert(err, IsNil)
	setDefaultPassword(c, m)
	add(m)

	_, err = s.State.AddService("mysql", s.AddTestingCharm(c, "mysql"))
	c.Assert(err, IsNil)

	wordpress, err := s.State.AddService("wordpress", s.AddTestingCharm(c, "wordpress"))
	c.Assert(err, IsNil)

	_, err = s.State.AddService("logging", s.AddTestingCharm(c, "logging"))
	c.Assert(err, IsNil)

	eps, err := s.State.InferEndpoints([]string{"logging", "wordpress"})
	c.Assert(err, IsNil)
	rel, err := s.State.AddRelation(eps...)
	c.Assert(err, IsNil)

	for i := 0; i < 2; i++ {
		wu, err := wordpress.AddUnit()
		c.Assert(err, IsNil)
		c.Assert(wu.Tag(), Equals, fmt.Sprintf("unit-wordpress-%d", i))
		setDefaultPassword(c, wu)
		add(wu)

		m, err := s.State.AddMachine("series", state.JobHostUnits)
		c.Assert(err, IsNil)
		c.Assert(m.Tag(), Equals, fmt.Sprintf("machine-%d", i+1))
		err = m.SetProvisioned(state.InstanceId("i-"+m.Tag()), "fake_nonce")
		c.Assert(err, IsNil)
		setDefaultPassword(c, m)
		add(m)

		err = wu.AssignToMachine(m)
		c.Assert(err, IsNil)

		deployer, ok := wu.DeployerTag()
		c.Assert(ok, Equals, true)
		c.Assert(deployer, Equals, fmt.Sprintf("machine-%d", i+1))

		wru, err := rel.Unit(wu)
		c.Assert(err, IsNil)

		// Create the subordinate unit as a side-effect of entering
		// scope in the principal's relation-unit.
		err = wru.EnterScope(nil)
		c.Assert(err, IsNil)

		lu, err := s.State.Unit(fmt.Sprintf("logging/%d", i))
		c.Assert(err, IsNil)
		c.Assert(lu.IsPrincipal(), Equals, false)
		deployer, ok = lu.DeployerTag()
		c.Assert(ok, Equals, true)
		c.Assert(deployer, Equals, fmt.Sprintf("unit-wordpress-%d", i))
		setDefaultPassword(c, lu)
		add(lu)
	}
	return
}

// apiAuthenticator represents a simple authenticator object with only the
// SetPassword and Tag methods.  This will fit types from both the state
// and api packages, as those in the api package do not have PasswordValid().
type apiAuthenticator interface {
	state.Tagger
	SetPassword(string) error
}

func setDefaultPassword(c *C, e apiAuthenticator) {
	err := e.SetPassword(e.Tag() + " password")
	c.Assert(err, IsNil)
}

var badLoginTests = []struct {
	tag      string
	password string
	err      string
	code     string
}{{
	tag:      "user-admin",
	password: "wrong password",
	err:      "invalid entity name or password",
	code:     api.CodeUnauthorized,
}, {
	tag:      "user-foo",
	password: "password",
	err:      "invalid entity name or password",
	code:     api.CodeUnauthorized,
}, {
	tag:      "bar",
	password: "password",
	err:      `invalid entity tag "bar"`,
}}

func (s *suite) TestBadLogin(c *C) {
	_, info, err := s.APIConn.Environ.StateInfo()
	c.Assert(err, IsNil)
	for i, t := range badLoginTests {
		c.Logf("test %d; entity %q; password %q", i, t.tag, t.password)
		info.Tag = ""
		info.Password = ""
		func() {
			st, err := api.Open(info)
			c.Assert(err, IsNil)
			defer st.Close()

			_, err = st.Machine("0")
			c.Assert(err, ErrorMatches, "not logged in")
			c.Assert(api.ErrCode(err), Equals, api.CodeUnauthorized, Commentf("error %#v", err))

			_, err = st.Unit("foo/0")
			c.Assert(err, ErrorMatches, "not logged in")
			c.Assert(api.ErrCode(err), Equals, api.CodeUnauthorized)

			err = st.Login(t.tag, t.password)
			c.Assert(err, ErrorMatches, t.err)
			c.Assert(api.ErrCode(err), Equals, t.code)

			_, err = st.Machine("0")
			c.Assert(err, ErrorMatches, "not logged in")
			c.Assert(api.ErrCode(err), Equals, api.CodeUnauthorized)
		}()
	}
}

func (s *suite) TestClientStatus(c *C) {
	s.setUpScenario(c)
	status, err := s.APIState.Client().Status()
	c.Assert(err, IsNil)
	c.Assert(status, DeepEquals, scenarioStatus)
}

func (s *suite) TestClientServerSet(c *C) {
	dummy, err := s.State.AddService("dummy", s.AddTestingCharm(c, "dummy"))
	c.Assert(err, IsNil)
	err = s.APIState.Client().ServiceSet("dummy", map[string]string{
		"title":    "xxx",
		"username": "yyy",
	})
	c.Assert(err, IsNil)
	conf, err := dummy.Config()
	c.Assert(err, IsNil)
	c.Assert(conf.Map(), DeepEquals, map[string]interface{}{
		"title":    "xxx",
		"username": "yyy",
	})
}

func (s *suite) TestClientServiceSetYAML(c *C) {
	dummy, err := s.State.AddService("dummy", s.AddTestingCharm(c, "dummy"))
	c.Assert(err, IsNil)
	err = s.APIState.Client().ServiceSetYAML("dummy", "title: aaa\nusername: bbb")
	c.Assert(err, IsNil)
	conf, err := dummy.Config()
	c.Assert(err, IsNil)
	c.Assert(conf.Map(), DeepEquals, map[string]interface{}{
		"title":    "aaa",
		"username": "bbb",
	})
}

var clientCharmInfoTests = []struct {
	about string
	url   string
	err   string
}{
	{
		about: "retrieves charm info",
		url:   "local:series/wordpress-3",
	},
	{
		about: "invalid URL",
		url:   "not-valid",
		err:   `charm URL has invalid schema: "not-valid"`,
	},
	{
		about: "unknown charm",
		url:   "cs:missing/one-1",
		err:   `charm "cs:missing/one-1" not found`,
	},
}

func (s *suite) TestClientCharmInfo(c *C) {
	// Use wordpress for tests so that we can compare Provides and Requires.
	charm := s.AddTestingCharm(c, "wordpress")
	for i, t := range clientCharmInfoTests {
		c.Logf("test %d. %s", i, t.about)
		info, err := s.APIState.Client().CharmInfo(t.url)
		if t.err != "" {
			c.Assert(err, ErrorMatches, t.err)
			continue
		}
		c.Assert(err, IsNil)
		expected := &api.CharmInfo{
			Revision: charm.Revision(),
			URL:      charm.URL().String(),
			Config:   charm.Config(),
			Meta:     charm.Meta(),
		}
		c.Assert(info, DeepEquals, expected)
	}
}

func (s *suite) TestClientEnvironmentInfo(c *C) {
	conf, _ := s.State.EnvironConfig()
	info, err := s.APIState.Client().EnvironmentInfo()
	c.Assert(err, IsNil)
	c.Assert(info.DefaultSeries, Equals, conf.DefaultSeries())
	c.Assert(info.ProviderType, Equals, conf.Type())
	c.Assert(info.Name, Equals, conf.Name())
}

var clientAnnotationsTests = []struct {
	about    string
	initial  map[string]string
	input    map[string]string
	expected map[string]string
	err      string
}{
	{
		about:    "test setting an annotation",
		input:    map[string]string{"mykey": "myvalue"},
		expected: map[string]string{"mykey": "myvalue"},
	},
	{
		about:    "test setting multiple annotations",
		input:    map[string]string{"key1": "value1", "key2": "value2"},
		expected: map[string]string{"key1": "value1", "key2": "value2"},
	},
	{
		about:    "test overriding annotations",
		initial:  map[string]string{"mykey": "myvalue"},
		input:    map[string]string{"mykey": "another-value"},
		expected: map[string]string{"mykey": "another-value"},
	},
	{
		about: "test setting an invalid annotation",
		input: map[string]string{"invalid.key": "myvalue"},
		err:   `cannot update annotations on .*: invalid key "invalid.key"`,
	},
}

func (s *suite) TestClientAnnotations(c *C) {
	// Set up entities.
	service, err := s.State.AddService("dummy", s.AddTestingCharm(c, "dummy"))
	c.Assert(err, IsNil)
	unit, err := service.AddUnit()
	c.Assert(err, IsNil)
	machine, err := s.State.AddMachine("series", state.JobHostUnits)
	c.Assert(err, IsNil)
	environment, err := s.State.Environment()
	c.Assert(err, IsNil)
	entities := []state.TaggedAnnotator{service, unit, machine, environment}
	for i, t := range clientAnnotationsTests {
		for _, entity := range entities {
			id := entity.Tag()
			c.Logf("test %d. %s. entity %s", i, t.about, id)
			// Set initial entity annotations.
			err := entity.SetAnnotations(t.initial)
			c.Assert(err, IsNil)
			// Add annotations using the API call.
			err = s.APIState.Client().SetAnnotations(id, t.input)
			if t.err != "" {
				c.Assert(err, ErrorMatches, t.err)
				continue
			}
			// Check annotations are correctly set.
			dbann, err := entity.Annotations()
			c.Assert(err, IsNil)
			c.Assert(dbann, DeepEquals, t.expected)
			// Retrieve annotations using the API call.
			ann, err := s.APIState.Client().GetAnnotations(id)
			c.Assert(err, IsNil)
			// Check annotations are correctly returned.
			c.Assert(ann, DeepEquals, dbann)
			// Clean up annotations on the current entity.
			cleanup := make(map[string]string)
			for key := range dbann {
				cleanup[key] = ""
			}
			err = entity.SetAnnotations(cleanup)
			c.Assert(err, IsNil)
		}
	}
}

func (s *suite) TestClientAnnotationsBadEntity(c *C) {
	bad := []string{"", "machine", "-foo", "foo-", "---", "machine-jim", "unit-123", "unit-foo", "service-", "service-foo/bar"}
	expected := `invalid entity tag ".*"`
	for _, id := range bad {
		err := s.APIState.Client().SetAnnotations(id, map[string]string{"mykey": "myvalue"})
		c.Assert(err, ErrorMatches, expected)
		_, err = s.APIState.Client().GetAnnotations(id)
		c.Assert(err, ErrorMatches, expected)
	}
}

func (s *suite) TestMachineLogin(c *C) {
	stm, err := s.State.AddMachine("series", state.JobHostUnits)
	c.Assert(err, IsNil)
	err = stm.SetPassword("machine-password")
	c.Assert(err, IsNil)
	err = stm.SetProvisioned("i-foo", "fake_nonce")
	c.Assert(err, IsNil)

	_, info, err := s.APIConn.Environ.StateInfo()
	c.Assert(err, IsNil)

	info.Tag = stm.Tag()
	info.Password = "machine-password"

	st, err := api.Open(info)
	c.Assert(err, IsNil)
	defer st.Close()

	m, err := st.Machine(stm.Id())
	c.Assert(err, IsNil)

	instId, ok := m.InstanceId()
	c.Assert(ok, Equals, true)
	c.Assert(instId, Equals, "i-foo")
}

func (s *suite) TestMachineInstanceId(c *C) {
	stm, err := s.State.AddMachine("series", state.JobHostUnits)
	c.Assert(err, IsNil)
	setDefaultPassword(c, stm)

	// Normal users can't access Machines...
	m, err := s.APIState.Machine(stm.Id())
	c.Assert(err, ErrorMatches, "permission denied")
	c.Assert(api.ErrCode(err), Equals, api.CodeUnauthorized)
	c.Assert(m, IsNil)

	// ... so login as the machine.
	st := s.openAs(c, stm.Tag())
	defer st.Close()

	m, err = st.Machine(stm.Id())
	c.Assert(err, IsNil)

	instId, ok := m.InstanceId()
	c.Check(instId, Equals, "")
	c.Check(ok, Equals, false)

	err = stm.SetProvisioned("foo", "fake_nonce")
	c.Assert(err, IsNil)

	instId, ok = m.InstanceId()
	c.Check(instId, Equals, "")
	c.Check(ok, Equals, false)

	err = m.Refresh()
	c.Assert(err, IsNil)

	instId, ok = m.InstanceId()
	c.Check(ok, Equals, true)
	c.Assert(instId, Equals, "foo")
}

func (s *suite) TestMachineRefresh(c *C) {
	// Add a machine and get its instance id (it's empty at first).
	stm, err := s.State.AddMachine("series", state.JobHostUnits)
	c.Assert(err, IsNil)
	oldId, _ := stm.InstanceId()
	c.Assert(oldId, Equals, state.InstanceId(""))

	// Now open the state connection for that machine.
	setDefaultPassword(c, stm)
	st := s.openAs(c, stm.Tag())
	defer st.Close()

	// Get the machine through the API.
	m, err := st.Machine(stm.Id())
	c.Assert(err, IsNil)
	// Set the original machine's instance id and nonce.
	err = stm.SetProvisioned("foo", "fake_nonce")
	c.Assert(err, IsNil)
	newId, _ := stm.InstanceId()
	c.Assert(newId, Equals, state.InstanceId("foo"))

	// Get the instance id of the machine through the API,
	// it should match the oldId, before the refresh.
	mId, _ := m.InstanceId()
	c.Assert(state.InstanceId(mId), Equals, oldId)
	err = m.Refresh()
	c.Assert(err, IsNil)
	// Now the instance id should be the new one.
	mId, _ = m.InstanceId()
	c.Assert(state.InstanceId(mId), Equals, newId)
}

func (s *suite) TestMachineSetPassword(c *C) {
	stm, err := s.State.AddMachine("series", state.JobHostUnits)
	c.Assert(err, IsNil)
	setDefaultPassword(c, stm)

	st := s.openAs(c, stm.Tag())
	defer st.Close()
	m, err := st.Machine(stm.Id())
	c.Assert(err, IsNil)

	err = m.SetPassword("foo")
	c.Assert(err, IsNil)

	err = stm.Refresh()
	c.Assert(err, IsNil)
	c.Assert(stm.PasswordValid("foo"), Equals, true)
}

func (s *suite) TestMachineTag(c *C) {
	c.Assert(api.MachineTag("2"), Equals, "machine-2")

	stm, err := s.State.AddMachine("series", state.JobHostUnits)
	c.Assert(err, IsNil)
	setDefaultPassword(c, stm)
	st := s.openAs(c, "machine-0")
	defer st.Close()
	m, err := st.Machine("0")
	c.Assert(err, IsNil)
	c.Assert(m.Tag(), Equals, "machine-0")
}

func (s *suite) TestMachineWatch(c *C) {
	stm, err := s.State.AddMachine("series", state.JobHostUnits)
	c.Assert(err, IsNil)
	setDefaultPassword(c, stm)

	st := s.openAs(c, stm.Tag())
	defer st.Close()
	m, err := st.Machine(stm.Id())
	c.Assert(err, IsNil)
	w0 := m.Watch()
	w1 := m.Watch()

	// Initial event.
	ok := chanRead(c, w0.Changes(), "watcher 0")
	c.Assert(ok, Equals, true)

	ok = chanRead(c, w1.Changes(), "watcher 1")
	c.Assert(ok, Equals, true)

	// No subsequent event until something changes.
	select {
	case <-w0.Changes():
		c.Fatalf("unexpected value on watcher 0")
	case <-w1.Changes():
		c.Fatalf("unexpected value on watcher 1")
	case <-time.After(20 * time.Millisecond):
	}

	err = stm.SetProvisioned("foo", "fake_nonce")
	c.Assert(err, IsNil)
	s.State.StartSync()

	// Next event.
	ok = chanRead(c, w0.Changes(), "watcher 0")
	c.Assert(ok, Equals, true)
	ok = chanRead(c, w1.Changes(), "watcher 1")
	c.Assert(ok, Equals, true)

	err = w0.Stop()
	c.Check(err, IsNil)
	err = w1.Stop()
	c.Check(err, IsNil)

	ok = chanRead(c, w0.Changes(), "watcher 0")
	c.Assert(ok, Equals, false)
	ok = chanRead(c, w1.Changes(), "watcher 1")
	c.Assert(ok, Equals, false)
}

func (s *suite) TestServerStopsOutstandingWatchMethod(c *C) {
	// Start our own instance of the server so we have
	// a handle on it to stop it.
	srv, err := apiserver.NewServer(s.State, "localhost:0", []byte(coretesting.ServerCert), []byte(coretesting.ServerKey))
	c.Assert(err, IsNil)

	stm, err := s.State.AddMachine("series", state.JobHostUnits)
	c.Assert(err, IsNil)
	err = stm.SetPassword("password")
	c.Assert(err, IsNil)

	// Note we can't use openAs because we're
	// not connecting to s.APIConn.
	st, err := api.Open(&api.Info{
		Tag:      stm.Tag(),
		Password: "password",
		Addrs:    []string{srv.Addr()},
		CACert:   []byte(coretesting.CACert),
	})
	c.Assert(err, IsNil)
	defer st.Close()

	m, err := st.Machine(stm.Id())
	c.Assert(err, IsNil)
	c.Assert(m.Id(), Equals, stm.Id())

	w := m.Watch()

	// Initial event.
	ok := chanRead(c, w.Changes(), "watcher 0")
	c.Assert(ok, Equals, true)

	// Wait long enough for the Next request to be sent
	// so it's blocking on the server side.
	time.Sleep(50 * time.Millisecond)
	c.Logf("stopping server")
	err = srv.Stop()
	c.Assert(err, IsNil)

	c.Logf("server stopped")
	ok = chanRead(c, w.Changes(), "watcher 0")
	c.Assert(ok, Equals, false)

	c.Assert(api.ErrCode(w.Err()), Equals, api.CodeStopped)
}

func chanRead(c *C, ch <-chan struct{}, what string) (ok bool) {
	select {
	case _, ok := <-ch:
		return ok
	case <-time.After(10 * time.Second):
		c.Fatalf("timed out reading from %s", what)
	}
	panic("unreachable")
}

func (s *suite) TestUnitRefresh(c *C) {
	s.setUpScenario(c)
	st := s.openAs(c, "unit-wordpress-0")
	defer st.Close()

	u, err := st.Unit("wordpress/0")
	c.Assert(err, IsNil)

	deployer, ok := u.DeployerTag()
	c.Assert(ok, Equals, true)
	c.Assert(deployer, Equals, "machine-1")

	stu, err := s.State.Unit("wordpress/0")
	c.Assert(err, IsNil)
	err = stu.UnassignFromMachine()
	c.Assert(err, IsNil)

	deployer, ok = u.DeployerTag()
	c.Assert(ok, Equals, true)
	c.Assert(deployer, Equals, "machine-1")

	err = u.Refresh()
	c.Assert(err, IsNil)

	deployer, ok = u.DeployerTag()
	c.Assert(ok, Equals, false)
	c.Assert(deployer, Equals, "")
}

func (s *suite) TestErrors(c *C) {
	stm, err := s.State.AddMachine("series", state.JobHostUnits)
	c.Assert(err, IsNil)
	setDefaultPassword(c, stm)
	st := s.openAs(c, stm.Tag())
	defer st.Close()
	// By testing this single call, we test that the
	// error transformation function is correctly called
	// on error returns from the API apiserver. The transformation
	// function itself is tested below.
	_, err = st.Machine("99")
	c.Assert(api.ErrCode(err), Equals, api.CodeNotFound)
}

var errorTransformTests = []struct {
	err  error
	code string
}{{
	err:  state.NotFoundf("hello"),
	code: api.CodeNotFound,
}, {
	err:  state.Unauthorizedf("hello"),
	code: api.CodeUnauthorized,
}, {
	err:  state.ErrCannotEnterScopeYet,
	code: api.CodeCannotEnterScopeYet,
}, {
	err:  state.ErrCannotEnterScope,
	code: api.CodeCannotEnterScope,
}, {
	err:  state.ErrExcessiveContention,
	code: api.CodeExcessiveContention,
}, {
	err:  state.ErrUnitHasSubordinates,
	code: api.CodeUnitHasSubordinates,
}, {
	err:  apiserver.ErrBadId,
	code: api.CodeNotFound,
}, {
	err:  apiserver.ErrBadCreds,
	code: api.CodeUnauthorized,
}, {
	err:  apiserver.ErrPerm,
	code: api.CodeUnauthorized,
}, {
	err:  apiserver.ErrNotLoggedIn,
	code: api.CodeUnauthorized,
}, {
	err:  apiserver.ErrUnknownWatcher,
	code: api.CodeNotFound,
}, {
	err:  &state.NotAssignedError{&state.Unit{}}, // too sleazy?!
	code: api.CodeNotAssigned,
}, {
	err:  apiserver.ErrStoppedWatcher,
	code: api.CodeStopped,
}, {
	err:  errors.New("an error"),
	code: "",
}}

func (s *suite) TestErrorTransform(c *C) {
	for _, t := range errorTransformTests {
		err1 := apiserver.ServerError(t.err)
		c.Assert(err1.Error(), Equals, t.err.Error())
		if t.code != "" {
			c.Assert(api.ErrCode(err1), Equals, t.code)
		} else {
			c.Assert(err1, Equals, t.err)
		}
	}
}

func (s *suite) TestUnitTag(c *C) {
	c.Assert(api.UnitTag("wordpress/2"), Equals, "unit-wordpress-2")

	s.setUpScenario(c)
	st := s.openAs(c, "unit-wordpress-0")
	defer st.Close()
	u, err := st.Unit("wordpress/0")
	c.Assert(err, IsNil)
	c.Assert(u.Tag(), Equals, "unit-wordpress-0")
}

func (s *suite) TestStop(c *C) {
	// Start our own instance of the server so we have
	// a handle on it to stop it.
	srv, err := apiserver.NewServer(s.State, "localhost:0", []byte(coretesting.ServerCert), []byte(coretesting.ServerKey))
	c.Assert(err, IsNil)

	stm, err := s.State.AddMachine("series", state.JobHostUnits)
	c.Assert(err, IsNil)
	err = stm.SetProvisioned("foo", "fake_nonce")
	c.Assert(err, IsNil)
	err = stm.SetPassword("password")
	c.Assert(err, IsNil)

	// Note we can't use openAs because we're
	// not connecting to s.APIConn.
	st, err := api.Open(&api.Info{
		Tag:      stm.Tag(),
		Password: "password",
		Addrs:    []string{srv.Addr()},
		CACert:   []byte(coretesting.CACert),
	})
	c.Assert(err, IsNil)
	defer st.Close()

	m, err := st.Machine(stm.Id())
	c.Assert(err, IsNil)
	c.Assert(m.Id(), Equals, stm.Id())

	err = srv.Stop()
	c.Assert(err, IsNil)

	_, err = st.Machine(stm.Id())
	// The client has not necessarily seen the server
	// shutdown yet, so there are two possible
	// errors.
	if err != rpc.ErrShutdown && err != io.ErrUnexpectedEOF {
		c.Fatalf("unexpected error from request: %v", err)
	}

	// Check it can be stopped twice.
	err = srv.Stop()
	c.Assert(err, IsNil)
}

func (s *suite) TestClientServiceGet(c *C) {
	s.setUpScenario(c)
	results, err := s.APIState.Client().ServiceGet("wordpress")
	c.Assert(err, IsNil)
	c.Assert(results, DeepEquals, &params.ServiceGetResults{
		Service: "wordpress",
		Charm:   "wordpress",
		Config: map[string]interface{}{
			"blog-title": map[string]interface{}{
				"type":        "string",
				"value":       nil,
				"description": "A descriptive title used for the blog."},
		},
	})
}

func (s *suite) TestClientServiceExpose(c *C) {
	s.setUpScenario(c)
	serviceName := "wordpress"
	service, err := s.State.Service(serviceName)
	c.Assert(err, IsNil)
	c.Assert(service.IsExposed(), Equals, false)
	err = s.APIState.Client().ServiceExpose(serviceName)
	c.Assert(err, IsNil)
	err = service.Refresh()
	c.Assert(err, IsNil)
	c.Assert(service.IsExposed(), Equals, true)
}

func (s *suite) TestClientServiceUnexpose(c *C) {
	s.setUpScenario(c)
	serviceName := "wordpress"
	service, err := s.State.Service(serviceName)
	c.Assert(err, IsNil)
	service.SetExposed()
	c.Assert(service.IsExposed(), Equals, true)
	err = s.APIState.Client().ServiceUnexpose(serviceName)
	c.Assert(err, IsNil)
	service.Refresh()
	c.Assert(service.IsExposed(), Equals, false)
}

func (s *suite) TestClientServiceDestroy(c *C) {
	// Setup:
	s.setUpScenario(c)
	serviceName := "wordpress"
	service, err := s.State.Service(serviceName)
	c.Assert(err, IsNil)
	// Code under test:
	err = s.APIState.Client().ServiceDestroy(serviceName)
	c.Assert(err, IsNil)
	err = service.Refresh()
	// The test actual assertion: the service should no-longer be Alive.
	c.Assert(service.Life(), Not(Equals), state.Alive)
}

func (s *suite) TestClientUnitResolved(c *C) {
	// Setup:
	s.setUpScenario(c)
	u, err := s.State.Unit("wordpress/0")
	c.Assert(err, IsNil)
	err = u.SetStatus(params.UnitError, "gaaah")
	c.Assert(err, IsNil)
	// Code under test:
	err = s.APIState.Client().Resolved("wordpress/0", false)
	c.Assert(err, IsNil)
	// Freshen the unit's state.
	err = u.Refresh()
	c.Assert(err, IsNil)
	// And now the actual test assertions: we set the unit as resolved via
	// the API so it should have a resolved mode set.
	mode := u.Resolved()
	c.Assert(mode, Equals, params.ResolvedNoHooks)
}

var serviceDeployTests = []struct {
	about            string
	serviceName      string
	charmUrl         string
	numUnits         int
	expectedNumUnits int
	constraints      constraints.Value
}{{
	about:            "Normal deploy",
	serviceName:      "mywordpress",
	charmUrl:         "local:series/wordpress",
	expectedNumUnits: 1,
	constraints:      constraints.MustParse("mem=1G"),
}, {
	about:            "Two units",
	serviceName:      "mywordpress",
	charmUrl:         "local:series/wordpress",
	numUnits:         2,
	expectedNumUnits: 2,
	constraints:      constraints.MustParse("mem=4G"),
},
}

func (s *suite) TestClientServiceDeploy(c *C) {
	s.setUpScenario(c)

	for i, test := range serviceDeployTests {
		c.Logf("test %d; %s", i, test.about)
		parsedUrl := charm.MustParseURL(test.charmUrl)
		localRepo, err := charm.InferRepository(parsedUrl, coretesting.Charms.Path)
		c.Assert(err, IsNil)
		withRepo(localRepo, func() {
			_, err = s.State.Service(test.serviceName)
			c.Assert(state.IsNotFound(err), Equals, true)
			err = s.APIState.Client().ServiceDeploy(
				test.charmUrl, test.serviceName, test.numUnits, "", test.constraints,
			)
			c.Assert(err, IsNil)

			service, err := s.State.Service(test.serviceName)
			c.Assert(err, IsNil)
			defer removeServiceAndUnits(c, service)
			scons, err := service.Constraints()
			c.Assert(err, IsNil)
			c.Assert(scons, DeepEquals, test.constraints)

			units, err := service.AllUnits()
			c.Assert(err, IsNil)
			c.Assert(units, HasLen, test.expectedNumUnits)
			for _, unit := range units {
				mid, err := unit.AssignedMachineId()
				c.Assert(err, IsNil)
				machine, err := s.State.Machine(mid)
				c.Assert(err, IsNil)
				mcons, err := machine.Constraints()
				c.Assert(err, IsNil)
				c.Assert(mcons, DeepEquals, test.constraints)
			}
		})
	}
}

func withRepo(repo charm.Repository, f func()) {
	// Monkey-patch server repository.
	originalServerCharmStore := apiserver.CharmStore
	apiserver.CharmStore = repo
	defer func() {
		apiserver.CharmStore = originalServerCharmStore
	}()
	f()
}

func (s *suite) TestSuccessfulAddRelation(c *C) {
	s.setUpScenario(c)
	endpoints := []string{"wordpress", "mysql"}
	res, err := s.APIState.Client().AddRelation(endpoints...)
	c.Assert(err, IsNil)
	c.Assert(res.Endpoints["wordpress"].Name, Equals, "db")
	c.Assert(res.Endpoints["wordpress"].Interface, Equals, "mysql")
	c.Assert(res.Endpoints["wordpress"].Scope, Equals, charm.RelationScope("global"))
	c.Assert(res.Endpoints["mysql"].Name, Equals, "server")
	c.Assert(res.Endpoints["mysql"].Interface, Equals, "mysql")
	c.Assert(res.Endpoints["mysql"].Scope, Equals, charm.RelationScope("global"))
	for _, endpoint := range endpoints {
		svc, err := s.State.Service(endpoint)
		c.Assert(err, IsNil)
		rels, err := svc.Relations()
		c.Assert(err, IsNil)
		for _, rel := range rels {
			c.Assert(rel.Life(), Equals, state.Alive)
		}
	}
}

func (s *suite) TestSuccessfulDestroyRelation(c *C) {
	s.setUpScenario(c)
	endpoints := []string{"wordpress", "logging"}
	err := s.APIState.Client().DestroyRelation(endpoints...)
	c.Assert(err, IsNil)
	for _, endpoint := range endpoints {
		service, err := s.State.Service(endpoint)
		c.Assert(err, IsNil)
		rels, err := service.Relations()
		c.Assert(err, IsNil)
		// When relations are destroyed they don't go away immediately but
		// instead are set to 'Dying', due to references held by the user
		// agent.
		for _, rel := range rels {
			c.Assert(rel.Life(), Equals, state.Dying)
		}
	}
}

func (s *suite) TestNoRelation(c *C) {
	s.setUpScenario(c)
	err := s.APIState.Client().DestroyRelation("wordpress", "mysql")
	c.Assert(err, ErrorMatches, `relation "wordpress:db mysql:server" not found`)
}

func (s *suite) TestClientWatchAll(c *C) {
	// A very simple end-to-end test, because
	// all the logic is tested elsewhere.
	m, err := s.State.AddMachine("series", state.JobManageEnviron)
	c.Assert(err, IsNil)
	err = m.SetProvisioned("i-0", state.BootstrapNonce)
	c.Assert(err, IsNil)
	watcher, err := s.APIState.Client().WatchAll()
	c.Assert(err, IsNil)
	defer func() {
		err := watcher.Stop()
		c.Assert(err, IsNil)
	}()
	deltas, err := watcher.Next()
	c.Assert(err, IsNil)
	if !c.Check(deltas, DeepEquals, []params.Delta{{
		Entity: &params.MachineInfo{
			Id:         m.Id(),
			InstanceId: "i-0",
			Status:     params.MachinePending,
		},
	}}) {
		c.Logf("got:")
		for _, d := range deltas {
			c.Logf("%#v\n", d.Entity)
		}
	}
}

// openAs connects to the API state as the given entity
// with the default password for that entity.
func (s *suite) openAs(c *C, tag string) *api.State {
	_, info, err := s.APIConn.Environ.StateInfo()
	c.Assert(err, IsNil)
	info.Tag = tag
	info.Password = fmt.Sprintf("%s password", tag)
	c.Logf("opening state; entity %q; password %q", info.Tag, info.Password)
	st, err := api.Open(info)
	c.Assert(err, IsNil)
	c.Assert(st, NotNil)
	return st
}
