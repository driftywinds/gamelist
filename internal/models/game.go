package models

// Canonical store identifiers used across the database, the sync engine and
// the JSON output. Display names live in DisplayStoreName. Using canonical
// lowercase keys everywhere avoids the "steam" vs "Steam" duplicate-row
// class of bugs.
const (
	StoreSteam     = "steam"
	StoreEpic      = "epic"
	StoreGOG       = "gog"
	StoreUbisoft   = "ubisoft"
	StoreXbox      = "xbox"
	StoreBattleNet = "battlenet"
	StoreDLsite    = "dlsite"
)

var storeDisplayNames = map[string]string{
	StoreSteam:     "Steam",
	StoreEpic:      "Epic Games Store",
	StoreGOG:       "GOG",
	StoreUbisoft:   "Ubisoft Connect",
	StoreXbox:      "Xbox",
	StoreBattleNet: "Battle.net",
	StoreDLsite:    "DLsite",
}

// DisplayStoreName maps a canonical store key to its human-readable name,
// falling back to the key itself for unknown stores.
func DisplayStoreName(key string) string {
	if name, ok := storeDisplayNames[key]; ok {
		return name
	}
	return key
}

// AllStoreKeys returns every supported store key in a stable order.
func AllStoreKeys() []string {
	return []string{StoreSteam, StoreEpic, StoreGOG, StoreUbisoft, StoreXbox, StoreBattleNet, StoreDLsite}
}

// Game is the enriched, cross-store view of one game.
//
// OwnedStores holds canonical store keys (sorted) so ownership survives JSON
// round-trips and database joins; presenters convert them with
// DisplayStoreName. StoreIDs maps a canonical store key to the store-specific
// game identifier (e.g. the Steam appid).
type Game struct {
	// ID is the matched IGDB game ID (0 when the game is not matched yet).
	ID int `json:"id"`

	// Title is the game's name as reported by the store it came from.
	Title string `json:"title"`

	// Description is the IGDB summary.
	Description string `json:"description,omitempty"`

	// ReleaseDate in YYYY-MM-DD format (from IGDB first_release_date, UTC).
	ReleaseDate string `json:"release_date,omitempty"`

	// Developers / Publishers / Platforms come from IGDB.
	Developers []string `json:"developers,omitempty"`
	Publishers []string `json:"publishers,omitempty"`
	Platforms  []string `json:"platforms,omitempty"`

	// OwnedStores lists every canonical store key that owns this game.
	OwnedStores []string `json:"owned_stores"`

	// StoreIDs maps canonical store keys to store-specific game IDs.
	StoreIDs map[string]string `json:"store_ids,omitempty"`
}
