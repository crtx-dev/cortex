package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	coreauth "github.com/gantry-tools/gantry-core/auth"
)

const accountSchemaVersion = 1

var cortexCapabilities = []coreauth.CapabilityInfo{
	{Key: "agent.run", Group: "Cortex", Label: "Run the coding agent"},
	{Key: "conversations.read-own", Group: "Cortex", Label: "Use own conversations"},
	{Key: "workspaces.use", Group: "Cortex", Label: "Use workspaces"},
	{Key: "providers.use-managed", Group: "Providers", Label: "Use managed providers"},
	{Key: "providers.manage", Group: "Providers", Label: "Manage provider credentials"},
	{Key: "accounts.manage", Group: "Cortex", Label: "Manage accounts"},
	{Key: "roles.manage", Group: "Cortex", Label: "Manage roles and permissions"},
	{Key: "launcher.configure.all", Group: "Launcher", Label: "Manage launcher instances"},
}

type cortexAccounts struct{ model *coreauth.Model }
type cortexAccountPersistence struct{ dir string }

func defaultCortexRoles() coreauth.RolesFile {
	return coreauth.RolesFile{Version: accountSchemaVersion, Roles: []coreauth.Role{
		{ID: "administrator", Name: "Administrator", Capabilities: []string{"*"}, BuiltIn: true},
		{ID: "user", Name: "User", Capabilities: []string{"agent.run", "conversations.read-own", "providers.use-managed", "workspaces.use"}, BuiltIn: true},
	}}
}

func cortexAccountPolicy() coreauth.AccountPolicy {
	return coreauth.AccountPolicy{SchemaVersion: accountSchemaVersion, ProductName: "Cortex", KnownCapability: func(key string) bool {
		for _, item := range cortexCapabilities {
			if item.Key == key {
				return true
			}
		}
		return false
	}}
}

func loadCortexAccounts(dir string) (*cortexAccounts, error) {
	model, err := coreauth.NewModel(cortexAccountPersistence{dir: dir}, cortexAccountPolicy())
	if err != nil {
		return nil, err
	}
	return &cortexAccounts{model: model}, nil
}

func (p cortexAccountPersistence) LoadAccounts() (coreauth.AccountsFile, error) {
	value := coreauth.AccountsFile{Version: accountSchemaVersion, Accounts: []coreauth.Account{}}
	err := readAccountFile(filepath.Join(p.dir, "users.json"), &value)
	if errors.Is(err, os.ErrNotExist) {
		return value, nil
	}
	return value, err
}

func (p cortexAccountPersistence) LoadRoles() (coreauth.RolesFile, error) {
	value := defaultCortexRoles()
	err := readAccountFile(filepath.Join(p.dir, "roles.json"), &value)
	if errors.Is(err, os.ErrNotExist) {
		return value, nil
	}
	return value, err
}

func (p cortexAccountPersistence) SaveAccounts(value coreauth.AccountsFile) error {
	if err := writeAccountFile(filepath.Join(p.dir, "users.json"), value); err != nil {
		return err
	}
	rolesPath := filepath.Join(p.dir, "roles.json")
	if _, err := os.Stat(rolesPath); errors.Is(err, os.ErrNotExist) {
		return writeAccountFile(rolesPath, defaultCortexRoles())
	}
	return nil
}

func (p cortexAccountPersistence) SaveRoles(value coreauth.RolesFile) error {
	return writeAccountFile(filepath.Join(p.dir, "roles.json"), value)
}

func readAccountFile(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func writeAccountFile(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary := path + ".tmp"
	if err = os.WriteFile(temporary, data, 0600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func (store *cortexAccounts) empty() bool { return store.model.Empty() }

func (store *cortexAccounts) list() []coreauth.Account {
	out := store.model.Accounts()
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].DisplayName) < strings.ToLower(out[j].DisplayName) })
	return out
}

func (store *cortexAccounts) roleList() []coreauth.Role { return store.model.Roles() }
func (store *cortexAccounts) account(id string) (coreauth.Account, bool) {
	return store.model.Account(id)
}
func (store *cortexAccounts) authenticate(username, password string) (coreauth.Account, coreauth.Identity, bool) {
	return store.model.AuthenticatePassword(username, password)
}
func (store *cortexAccounts) capabilities(id string) []string { return store.model.Capabilities(id) }

func (store *cortexAccounts) create(display, username, password string, roleIDs []string) (coreauth.Account, error) {
	if len(roleIDs) == 0 {
		roleIDs = []string{"user"}
	}
	return store.model.CreateAccount(display, username, password, roleIDs)
}

func (store *cortexAccounts) initial(display, username, password string) (coreauth.Account, error) {
	return store.model.CreateInitialAdministrator(display, username, password)
}

func (store *cortexAccounts) update(id, display string, enabled bool, roleIDs []string) error {
	return store.model.UpdateAccount(id, display, enabled, roleIDs)
}

func (store *cortexAccounts) resetPassword(id, password string) error {
	account, found := store.model.Account(id)
	if !found {
		return errors.New("account not found")
	}
	for _, identity := range account.Identities {
		if identity.Type == "password" {
			return store.model.SetPassword(id, identity.ID, password)
		}
	}
	return errors.New("password identity not found")
}
