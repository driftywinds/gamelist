# Game List Manager

This project is a Go-based CLI application that manages a user's game library across multiple digital storefronts.

## Features

*   **Multi-Store Integration:** Connect to various game stores (Steam, Epic Games Store, GOG, Ubisoft Connect, Xbox, Battle.net, DLsite) to retrieve owned games.
*   **Game Data Enrichment:** Utilize the IGDB API to fetch comprehensive game data, including release dates, developers, publishers, and platforms.
*   **Cross-Store Game Matching:** Identify and display when a game is owned across multiple stores.
*   **Local Data Storage:** Use SQLite to store user data and game library information locally.
*   **API-First Design:** Structure the application with an API-like interface to facilitate the development of future frontends.

## Project Structure

```
game-list-manager/
├── cmd/
│   └── main.go         # Main entry point for the CLI application
├── internal/
│   ├── api/
│   │   ├── igdb.go         # Handles communication with the IGDB API
│   │   └── store_apis.go   # Interfaces for different game store APIs
│   ├── database/
│   │   └── sqlite.go       # SQLite database interaction logic
│   ├── models/
│   │   └── game.go         # Data structures for games and user data
│   └── services/
│       ├── game_service.go # Business logic for game retrieval and matching
│       └── auth_service.go # Handles user authentication for stores
├── pkg/
│   └── api/
│       ├── client.go       # Generic API client
│       └── errors.go       # Custom API error types
├── migrations/
│   └── 001_create_tables.sql # Database migration scripts
├── configs/
│   └── config.yaml       # Configuration file (API keys, store URLs, etc.)
├── go.mod
├── go.sum
└── README.md
```

## Setup Instructions

1.  **Prerequisites:**
    *   **Go:** Ensure you have Go installed (version 1.18 or later).
    *   **SQLite:** SQLite libraries should be available on your system. The Go driver will be included in the project.
    *   **IGDB API Key:** Obtain an API key from [IGDB.com](https://api-docs.igdb.com/#authentication)

2.  **Clone the Repository:**
    ```bash
    git clone https://github.com/yourusername/game-list-manager.git
    cd game-list-manager
    ```

3.  **Configure API Keys:**
    Create a `configs/config.yaml` file (if it doesn't exist) and add your IGDB API key:
    ```yaml
igdb:
  apiKey: YOUR_IGDB_API_KEY
```
    You will also need to configure credentials for each game store API. This might involve OAuth flows or API key generation depending on the store.

4.  **Install Dependencies:**
    ```bash
    go mod tidy
    ```

5.  **Database Setup:**
    The application will automatically create the SQLite database file (e.g., `gamelist.db`) on first run. You can also run migrations manually:
    ```bash
    # (Potentially a go generate command or a specific migration tool command)
    ```

## How it Works

1.  **Authentication:** The `auth_service` handles user authentication for each supported game store. This may involve interactive login flows or using pre-configured credentials.
2.  **Game Retrieval:** Once authenticated, the `store_apis` interfaces are used to fetch the list of games owned by the user from each store.
3.  **Data Enrichment:** The retrieved game data is then passed to the `igdb.go` handler, which queries the IGDB API to gather additional details about each game.
4.  **Data Storage:** All game data, including ownership information across stores and IGDB details, is stored in a local SQLite database.
5.  **Game Matching:** The `game_service` is responsible for matching games across different stores and enriching the data with IGDB information.
6.  **API Layer:** The core logic is exposed through functions that can be called by the CLI. This design aims to make it easy to build a separate frontend that can interact with these functions as if they were API endpoints.

## Debugging and Testing

### Debugging

*   **Logging:** Implement comprehensive logging throughout the application. Use Go's built-in `log` package or a more advanced logging library like `logrus` or `zap`.
*   **Print Statements:** Add `fmt.Println` statements at critical points to trace execution flow and variable values during development.
*   **Go Debugger:** Use a debugger like Delve (`dlv`) to step through the code, inspect variables, and set breakpoints.
    ```bash
    # Install Delve
    go install github.com/go-delve/delve/cmd/dlv@latest

    # Start your application in debug mode
    dlv debug cmd/main.go
    ```

### Testing

*   **Unit Tests:** Write unit tests for individual functions and components, especially for the API clients, database interactions, and business logic. Place them in files named `*_test.go` within their respective packages.
    ```go
    // Example: internal/services/game_service_test.go
    package services_test

    import (
        // ... imports
        "testing"
        "github.com/stretchr/testify/assert"
    )

    func TestMatchGamesAcrossStores(t *testing.T) {
        // setup mock dependencies
        // call the function under test
        // assert the results
    }
    ```
*   **Integration Tests:** Create integration tests that verify the interaction between different components, such as the service layer and the database, or the API clients and mock API endpoints.
*   **Debug Build:** Compile the application in debug mode to include debugging symbols, which can be helpful for profiling and in-depth debugging.
    ```bash
    go build -gcflags="all=-N -l" -o game-list-manager-debug cmd/main.go
    ```
    Then run the executable: `./game-list-manager-debug`

## Future Enhancements

*   **Frontend Development:** Build a web or desktop frontend using the exposed API functions.
*   **Platform Support:** Expand support for more game stores and platforms.
*   **User Interface:** Enhance the CLI UI with more user-friendly features (e.g., TUI libraries like `tview`).

## License

This project is licensed under the MIT License.
