package services

import (
	"encoding/json"
	"fmt"

	"gamelist/internal/api"
	"gamelist/internal/database"
	"gamelist/internal/models"
)

// AuthService runs sign-in flows and persists credentials in the local
// SQLite database. `sync` then reads credentials from the database, which
// makes `signin` a real, required step instead of a print statement.
type AuthService struct {
	db *database.DB
}

// NewAuthService creates an AuthService.
func NewAuthService(db *database.DB) *AuthService {
	return &AuthService{db: db}
}

// SignIn validates credentials via the store API and persists them locally.
func (s *AuthService) SignIn(storeAPI api.StoreAPI) error {
	if err := storeAPI.SignIn(); err != nil {
		return fmt.Errorf("%s sign-in: %w", storeAPI.Name(), err)
	}
	return s.Persist(storeAPI)
}

// Persist stores already-validated credentials in the local database.
// Used by `config signin`, which validates once and then persists.
func (s *AuthService) Persist(storeAPI api.StoreAPI) error {
	if err := s.persist(storeAPI); err != nil {
		return fmt.Errorf("%s: could not persist credentials: %w", storeAPI.Name(), err)
	}
	return nil
}

// persist writes store-specific credential data into store_credentials.
func (s *AuthService) persist(storeAPI api.StoreAPI) error {
	switch st := storeAPI.(type) {
	case *api.SteamAPI:
		meta, err := json.Marshal(map[string]string{
			"api_key":  st.APIKey(),
			"steam_id": st.SteamID(),
		})
		if err != nil {
			return err
		}
		return s.db.SaveStoreCredentials(models.StoreSteam, "", "", 0, string(meta))

	case *api.EpicGamesAPI:
		meta, err := json.Marshal(map[string]string{
			"client_id":     st.ClientID(),
			"client_secret": st.ClientSecret(),
		})
		if err != nil {
			return err
		}
		token, exp := st.Token()
		return s.db.SaveStoreCredentials(models.StoreEpic, token, "", exp, string(meta))

	case *api.BattleNetAPI:
		meta, err := json.Marshal(map[string]string{
			"client_id":     st.ClientID(),
			"client_secret": st.ClientSecret(),
		})
		if err != nil {
			return err
		}
		token, exp := st.Token()
		return s.db.SaveStoreCredentials(models.StoreBattleNet, token, "", exp, string(meta))

	default:
		return fmt.Errorf("no local credential storage defined for %s", storeAPI.Name())
	}
}

// SignOut removes stored credentials for a store.
func (s *AuthService) SignOut(storeName string) error {
	if err := s.db.ClearStoreCredentials(storeName); err != nil {
		return fmt.Errorf("sign out of %s: %w", storeName, err)
	}
	return nil
}
