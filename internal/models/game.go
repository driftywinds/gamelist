package models

// Game represents a game entity.
type Game struct {
	ID          int
	Title       string
	Description string
	ReleaseDate string // Consider using time.Time for proper date handling
	Developers  []string
	Publishers  []string
	Platforms   []string
	OwnedStores []string // List of stores where this game is owned
}

// User represents a user of the application.
type User struct {
	ID       int
	Username string
	// Other user-related fields
}

// StoreCredentials holds authentication details for a specific game store.
type StoreCredentials struct {
	StoreID      string // e.g., "steam", "epic"
	AccessToken  string
	RefreshToken string
	// Other authentication-related fields
}
