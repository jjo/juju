// Copyright 2016 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package user_test

import (
	"github.com/juju/cmd"
	"github.com/juju/errors"
	jc "github.com/juju/testing/checkers"
	gc "gopkg.in/check.v1"

	"github.com/juju/juju/cmd/juju/user"
	coretesting "github.com/juju/juju/testing"
)

type LogoutCommandSuite struct {
	BaseSuite
}

var _ = gc.Suite(&LogoutCommandSuite{})

func (s *LogoutCommandSuite) SetUpTest(c *gc.C) {
	s.BaseSuite.SetUpTest(c)
}

func (s *LogoutCommandSuite) run(c *gc.C, args ...string) (*cmd.Context, error) {
	cmd, _ := user.NewLogoutCommandForTest(s.store)
	return coretesting.RunCommand(c, cmd, args...)
}

func (s *LogoutCommandSuite) TestInit(c *gc.C) {
	for i, test := range []struct {
		args        []string
		errorString string
	}{
		{
		// no args is fine
		}, {
			args:        []string{"foobar"},
			errorString: `unrecognized args: \["foobar"\]`,
		}, {
			args:        []string{"--foobar"},
			errorString: "flag provided but not defined: --foobar",
		},
	} {
		c.Logf("test %d", i)
		wrappedCommand, _ := user.NewLogoutCommandForTest(s.store)
		err := coretesting.InitCommand(wrappedCommand, test.args)
		if test.errorString == "" {
			c.Check(err, jc.ErrorIsNil)
		} else {
			c.Check(err, gc.ErrorMatches, test.errorString)
		}
	}
}

func (s *LogoutCommandSuite) TestLogout(c *gc.C) {
	details := s.store.Accounts["testing"].Accounts["current-user@local"]
	details.Macaroon = "a-macaroon"
	s.store.Accounts["testing"].Accounts["current-user@local"] = details
	ctx, err := s.run(c)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(coretesting.Stdout(ctx), gc.Equals, "")
	c.Assert(coretesting.Stderr(ctx), gc.Equals, `
Logged out. You are no longer logged into any controllers.
`[1:],
	)
	_, err = s.store.CurrentAccount("testing")
	c.Assert(err, jc.Satisfies, errors.IsNotFound)
	_, err = s.store.AccountByName("testing", "current-user@local")
	c.Assert(err, jc.Satisfies, errors.IsNotFound)
}

func (s *LogoutCommandSuite) TestLogoutCount(c *gc.C) {
	// Create multiple controllers. We'll log out of each one
	// to observe the messages printed out by "logout".
	controllers := []string{"testing", "testing2", "testing3"}
	details := s.store.Accounts["testing"].Accounts["current-user@local"]
	details.Macaroon = "a-macaroon"
	for _, controller := range controllers {
		s.store.Controllers[controller] = s.store.Controllers["testing"]
		s.store.Controllers[controller] = s.store.Controllers["testing"]
		err := s.store.UpdateAccount(controller, "current-user@local", details)
		c.Assert(err, jc.ErrorIsNil)
		err = s.store.SetCurrentAccount(controller, "current-user@local")
		c.Assert(err, jc.ErrorIsNil)
	}

	expected := []string{
		"Logged out. You are still logged into 2 controllers.\n",
		"Logged out. You are still logged into 1 controller.\n",
		"Logged out. You are no longer logged into any controllers.\n",
	}

	for i, controller := range controllers {
		ctx, err := s.run(c, "-c", controller)
		c.Assert(err, jc.ErrorIsNil)
		c.Assert(coretesting.Stdout(ctx), gc.Equals, "")
		c.Assert(coretesting.Stderr(ctx), gc.Equals, expected[i])
	}
}

func (s *LogoutCommandSuite) TestLogoutWithoutMacaroon(c *gc.C) {
	s.assertStorePassword(c, "current-user@local", "old-password")
	s.assertStoreMacaroon(c, "current-user@local", nil)
	_, err := s.run(c)
	c.Assert(err, gc.NotNil)
	c.Assert(err.Error(), gc.Equals, `preventing account loss

It appears that you have not changed the password for
your account. If this is the case, change the password
first before logging out, so that you can log in again
afterwards. To change your password, run the command
"juju change-user-password".

If you are sure you want to log out, and it is safe to
clear the credentials from the client, then you can run
this command again with the "--force" flag.
`)
}

func (s *LogoutCommandSuite) TestLogoutWithoutMacaroonForced(c *gc.C) {
	s.assertStorePassword(c, "current-user@local", "old-password")
	s.assertStoreMacaroon(c, "current-user@local", nil)
	_, err := s.run(c, "--force")
	c.Assert(err, jc.ErrorIsNil)
	_, err = s.store.CurrentAccount("testing")
	c.Assert(err, jc.Satisfies, errors.IsNotFound)
}

func (s *LogoutCommandSuite) TestLogoutNotLoggedIn(c *gc.C) {
	s.store.Accounts["testing"].CurrentAccount = ""
	ctx, err := s.run(c)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(coretesting.Stdout(ctx), gc.Equals, "")
	c.Assert(coretesting.Stderr(ctx), gc.Equals, `
Logged out. You are no longer logged into any controllers.
`[1:],
	)
}
