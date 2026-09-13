package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

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

type cortexAccounts struct {
	mu    sync.RWMutex
	dir   string
	users coreauth.AccountsFile
	roles coreauth.RolesFile
}

func defaultCortexRoles() coreauth.RolesFile {
	return coreauth.RolesFile{Version: accountSchemaVersion, Roles: []coreauth.Role{
		{ID: "administrator", Name: "Administrator", Capabilities: []string{"*"}, BuiltIn: true},
		{ID: "user", Name: "User", Capabilities: []string{"agent.run", "conversations.read-own", "providers.use-managed", "workspaces.use"}, BuiltIn: true},
	}}
}

func loadCortexAccounts(dir string) (*cortexAccounts, error) {
	store := &cortexAccounts{
		dir:   dir,
		users: coreauth.AccountsFile{Version: accountSchemaVersion, Accounts: []coreauth.Account{}},
		roles: defaultCortexRoles(),
	}
	if err := readAccountFile(filepath.Join(dir, "users.json"), &store.users); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := readAccountFile(filepath.Join(dir, "roles.json"), &store.roles); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := coreauth.ValidateAccounts(store.users, store.roles, store.policy()); err != nil {
		return nil, err
	}
	return store, nil
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

func (store *cortexAccounts) policy() coreauth.AccountPolicy {
	return coreauth.AccountPolicy{SchemaVersion: accountSchemaVersion, ProductName: "Cortex", KnownCapability: func(key string) bool {
		for _, item := range cortexCapabilities {
			if item.Key == key {
				return true
			}
		}
		return false
	}}
}

func (store *cortexAccounts) empty() bool {
	store.mu.RLock()
	defer store.mu.RUnlock()
	return len(store.users.Accounts) == 0
}

func (store *cortexAccounts) list() []coreauth.Account {
	store.mu.RLock()
	defer store.mu.RUnlock()
	out := append([]coreauth.Account(nil), store.users.Accounts...)
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].DisplayName) < strings.ToLower(out[j].DisplayName) })
	return out
}

func (store *cortexAccounts) roleList() []coreauth.Role {
	store.mu.RLock()
	defer store.mu.RUnlock()
	return append([]coreauth.Role(nil), store.roles.Roles...)
}

func (store *cortexAccounts) account(id string) (coreauth.Account, bool) {
	for _, item := range store.list() {
		if item.ID == id {
			return item, true
		}
	}
	return coreauth.Account{}, false
}

func (store *cortexAccounts) authenticate(username, password string) (coreauth.Account, coreauth.Identity, bool) {
	for _, account := range store.list() {
		if !account.Enabled {
			continue
		}
		for _, identity := range account.Identities {
			if identity.Enabled && identity.Type == "password" && strings.EqualFold(identity.Username, strings.TrimSpace(username)) && coreauth.VerifyPassword(identity.PasswordHash, password) {
				return account, identity, true
			}
		}
	}
	return coreauth.Account{}, coreauth.Identity{}, false
}

func (store *cortexAccounts) capabilities(id string) []string {
	account, ok := store.account(id)
	if !ok {
		return nil
	}
	return coreauth.EffectiveCapabilities(account, store.roleList())
}

func (store *cortexAccounts) create(display, username, password string, roleIDs []string) (coreauth.Account, error) {
	display, username = strings.TrimSpace(display), strings.TrimSpace(username)
	if display == "" || username == "" || len(password) < 7 {
		return coreauth.Account{}, errors.New("display name, username and a password of at least 7 characters are required")
	}
	hash, err := coreauth.HashPassword(password)
	if err != nil {
		return coreauth.Account{}, err
	}
	if len(roleIDs) == 0 {
		roleIDs = []string{"user"}
	}
	account := coreauth.Account{ID: coreauth.NewID("acct"), DisplayName: display, Enabled: true, Roles: coreauth.DedupeStrings(roleIDs), CreatedAt: time.Now().UTC(), Identities: []coreauth.Identity{{ID: coreauth.NewID("id"), Type: "password", Username: username, PasswordHash: hash, Enabled: true}}}
	store.mu.Lock()
	defer store.mu.Unlock()
	next := store.users
	next.Accounts = append(append([]coreauth.Account(nil), store.users.Accounts...), account)
	if err = coreauth.ValidateAccounts(next, store.roles, store.policy()); err != nil {
		return coreauth.Account{}, err
	}
	if err = writeAccountFile(filepath.Join(store.dir, "users.json"), next); err != nil {
		return coreauth.Account{}, err
	}
	if _, statErr := os.Stat(filepath.Join(store.dir, "roles.json")); errors.Is(statErr, os.ErrNotExist) {
		if err = writeAccountFile(filepath.Join(store.dir, "roles.json"), store.roles); err != nil {
			return coreauth.Account{}, err
		}
	}
	store.users = next
	return account, nil
}

func (store *cortexAccounts) initial(display, username, password string) (coreauth.Account, error) {
	if !store.empty() {
		return coreauth.Account{}, errors.New("setup is already complete")
	}
	return store.create(display, username, password, []string{"administrator"})
}

func (store *cortexAccounts) update(id, display string, enabled bool, roleIDs []string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	next := store.users
	next.Accounts = append([]coreauth.Account(nil), store.users.Accounts...)
	found := false
	for index := range next.Accounts {
		if next.Accounts[index].ID == id {
			found = true
			next.Accounts[index].DisplayName = strings.TrimSpace(display)
			next.Accounts[index].Enabled = enabled
			next.Accounts[index].Roles = coreauth.DedupeStrings(roleIDs)
		}
	}
	if !found {
		return errors.New("account not found")
	}
	if err := coreauth.ValidateAccounts(next, store.roles, store.policy()); err != nil {
		return err
	}
	if err := writeAccountFile(filepath.Join(store.dir, "users.json"), next); err != nil {
		return err
	}
	store.users = next
	return nil
}

func (store *cortexAccounts) resetPassword(id, password string) error {
	if len(password) < 7 {
		return errors.New("password must be at least 7 characters")
	}
	hash, err := coreauth.HashPassword(password)
	if err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	next := store.users
	next.Accounts = append([]coreauth.Account(nil), store.users.Accounts...)
	found := false
	for accountIndex := range next.Accounts {
		if next.Accounts[accountIndex].ID == id {
			next.Accounts[accountIndex].Identities = append([]coreauth.Identity(nil), next.Accounts[accountIndex].Identities...)
			for identityIndex := range next.Accounts[accountIndex].Identities {
				identity := &next.Accounts[accountIndex].Identities[identityIndex]
				if identity.Type == "password" {
					identity.PasswordHash = hash
					found = true
					break
				}
			}
		}
	}
	if !found {
		return errors.New("password identity not found")
	}
	if err = writeAccountFile(filepath.Join(store.dir, "users.json"), next); err == nil {
		store.users = next
	}
	return err
}
