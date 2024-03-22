package main

/* Instructions
1. Initialize Dependencies
	go mod init myapp
	go get github.com/pocketbase/pocketbase
	go mod tidy
2. Get Migrations [If no pb_data folder]
	go run main.go migrate up
3. Create Admin [If no pb_data folder]
	go run main.go admin create "junyi.li@ucalgary.ca" "123123123123"
4. Run (requires auth)
	go run main.go serve
5. Tables [If no pb_data folder]
	For our app, once you sign in, import collections of frontend/pb_schema.json
	Automigrate will then create the needed folders in pb_data, and you can uncomment some things.
*/

import (
	"log"
	"os"

	"fmt"
	"net/http"
	"regexp"
	"sync"
	"time"

	"golang.org/x/net/websocket"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/plugins/migratecmd"

	// Uncomment once you have at least one .go migration file in the "pb_migrations directory"
	_ "myapp/pb_data/migrations"
	// Creating Records
	"github.com/pocketbase/pocketbase/forms"
	"github.com/pocketbase/pocketbase/models"
)

var PK = false
var connectedServers = make(map[*websocket.Conn]bool)

// handle Connections
func handleWebSocket(ws *websocket.Conn) {
	connectedServers[ws] = true
	log.Println("Server Connected: ", ws.RemoteAddr())

	// Keep Websocket Open
	select {}
}

// Sends a message to all connected servers
func broadcastMsg(message string) {
	for pb := range connectedServers {
		if err := websocket.Message.Send(pb, message); err != nil {
			log.Println("Error Sending Message: ", err)
			delete(connectedServers, pb)
		}
	}
}

func handleMessage(ws *websocket.Conn, app *pocketbase.PocketBase, wg *sync.WaitGroup) {
	defer wg.Done()

	for {
		// Attempt to Connect
		// host.docker.internal -> looks at host machine's localhost instead of containers
		// Note -- this still relies on localhost to succeed
		// This doesn't work purely with docker containers unless --network is ran with Docker.
		// We have two options:
		//	1. Connect to active LB, which sends a message of whose the primary -- connect to it
		//		-- Saves us the persistent checking, and keeps other replicas unknown from one-another
		//	2. Check all known replicas, who's hosting
		//		-- What's currently implemented here -- we keep checking until we connect.
		//		-- Operates under the assumption that LB only sends writes to primary.
		//			primary will create a server, so replicas can differentiate broadcast vs write
		//			based off of active websocket connection.
		if ws == nil && !PK {
			log.Println("Attempting to Connect to localhost:8081")
			var err error
			ws, err = websocket.Dial("ws://host.docker.internal:8081/ws", "", "http://localhost/")
			if err != nil {
				log.Println("Error connecting to localhost:8081:", err)
				time.Sleep(3 * time.Second) // Retry after 3 seconds
				continue
			}
			log.Println("Connected to localhost:8081")
		}

		if ws != nil {
			for {
				var message string
				err := websocket.Message.Receive(ws, &message)
				if err != nil {
					// Websocket Closure
					if err.Error() == "EOF" {
						log.Println("Connection Closed. Reconnecting...")
						ws.Close()
						ws = nil
						break
					}
					fmt.Println("Error receiving message: ", err)
					break
				}

				// Combined Regex Pattern [Message] and [User]
				// Note -- matches[3] appears due to a bug, but matches[4] is messageContent
				// [User] --> 1-Type, 2-ID, 5-Name
				// [Message] --> 1-Type, 2-ID, 4-Content, 5-Name
				pattern := `^([^:]+):([^:]+):((.*?):)?([^:]+)$`

				regex := regexp.MustCompile(pattern)
				matches := regex.FindStringSubmatch(message)

				// Testing - Print Contents
				// if len(matches) > 0 {
				// 	for i, match := range matches[1:] {
				// 		fmt.Printf("Group %d: %s\n", i+1, match)
				// 	}
				// } else {
				// 	fmt.Println("Matches is Empty")
				// }

				log.Println("Received Message: ", message)

				switch matches[1] {
				case "1":
					collection, err := app.Dao().FindCollectionByNameOrId("messages")
					if err != nil {
						log.Println("Error in Collection Finding")
					}

					record := models.NewRecord(collection)
					form := forms.NewRecordUpsert(app, record)

					form.LoadData(map[string]any{
						"id":      matches[2],
						"content": matches[4],
						"user":    matches[5],
					})

					// Validate and Submit
					if err := form.Submit(); err != nil {
						log.Println("Error in Submission")
					}
				case "2":
					collection, err := app.Dao().FindCollectionByNameOrId("users")
					if err != nil {
						log.Println("Error in Collection Finding")
					}

					record := models.NewRecord(collection)
					form := forms.NewRecordUpsert(app, record)

					form.LoadData(map[string]any{
						"id":       matches[2],
						"username": matches[5],
					})

					// Validate and Submit
					if err := form.Submit(); err != nil {
						log.Println("Error in Submission")
					}
				default:
					log.Println("Error has Occurred")
				}
			}
		} else {
			// Primary -- sleep for 100 seconds
			time.Sleep(100 * time.Second)
		}
	}
}

func main() {
	var wg sync.WaitGroup
	var ws *websocket.Conn
	port := ":8081"
	http.Handle("/ws", websocket.Handler(handleWebSocket))

	// Attempt to Connect -- as Client
	// Note: This code is entirely localhost-based.

	// New Pocketbase Instance
	app := pocketbase.New()

	// Start a Go Routine to handle messages
	wg.Add(1)
	go handleMessage(ws, app, &wg)

	// Serve Static files from the provided public dir (if exists)
	app.OnBeforeServe().Add(func(e *core.ServeEvent) error {
		e.Router.GET("/*", apis.StaticDirectoryHandler(os.DirFS("./pb_public"), false))
		return nil
	})

	// Idea -- This could be useful, to get the a most recent (auto-logged) migration file to use
	// in the creation of a new DB. Maybe instead of hardcoded -- leader is true, others is false.
	migratecmd.MustRegister(app, app.RootCmd, migratecmd.Config{
		// Enable autocreation of Migration Files when making collection changes in the Admin UI
		// (the isGoRun check is to enable it only during development)
		Dir:         "./pb_data/migrations",
		Automigrate: true,
	})

	app.OnRecordBeforeCreateRequest("messages", "users").Add(func(e *core.RecordCreateEvent) error {
		log.Println("Record Create Event Before messages | user")
		log.Println(e.HttpContext)
		log.Println(e.Record)
		log.Println(e.UploadedFiles)

		// If websocket isn't open and we're considered a replica, but we got a write request
		// Then we must be the primary -- thus, Host a server.
		// Wait 3s (check-down-time), proceed.
		if ws == nil && !PK {
			PK = true
			log.Println("No Active Connection -- We must be the Primary")
			go func() {
				err := http.ListenAndServe("0.0.0.0"+port, nil)
				if err != nil {
					log.Println("Server already running on port 8081")
				}
			}()
			time.Sleep(3 * time.Second)
		}

		return nil
	})

	// Record Creation Test
	// Note: This requires Collections 'messages' and 'users' to function
	app.OnRecordAfterCreateRequest("messages").Add(func(e *core.RecordCreateEvent) error {
		log.Println("Record Create Event for messages")
		log.Println(e.HttpContext)
		log.Println(e.Record)
		log.Println(e.UploadedFiles)

		if PK {
			log.Println("1:" + e.Record.Id + ":" + e.Record.OriginalCopy().GetString("content") + ":" + e.Record.OriginalCopy().GetString("user"))
			broadcastMsg("1:" + e.Record.Id + ":" + e.Record.OriginalCopy().GetString("content") + ":" + e.Record.OriginalCopy().GetString("user"))
		}

		return nil
	})

	app.OnRecordAfterCreateRequest("users").Add(func(e *core.RecordCreateEvent) error {
		log.Println("Record Create Event for users")
		log.Println(e.HttpContext)
		log.Println(e.Record)
		log.Println(e.UploadedFiles)

		if PK {
			log.Println("2:" + e.Record.Id + ":" + e.Record.OriginalCopy().GetString("username"))
			broadcastMsg("2:" + e.Record.Id + ":" + e.Record.OriginalCopy().GetString("username"))
		}

		return nil
	})

	// Log Errors that occur on execution (serve)
	if err := app.Start(); err != nil {
		log.Fatal(err)
	}

	// https://pocketbase.io/docs/go-routing/ --> HTTP Reading, likely needed to broadcast.
}
