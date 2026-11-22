// Command catconnect is the player app: paste a server's link and its game
// servers become reachable on localhost.
package main

import (
	"embed"
	"log"
	"os"
	"slices"

	"github.com/tailscale/tailcat"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

//go:embed all:frontend/dist
var assets embed.FS

// backgroundFlag starts CatConnect in the tray without showing its window,
// as used when it starts with the computer.
const backgroundFlag = "--background"

func main() {
	tailcat.Verbose = false
	a := NewApp()
	d := &desktop{}
	a.desktop = d

	app := application.New(application.Options{
		Name:        "CatConnect",
		Description: "Join your friends' game servers, powered by tailcat",
		Services:    []application.Service{application.NewService(a)},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Mac: application.MacOptions{
			ActivationPolicy: application.ActivationPolicyAccessory,
			ApplicationShouldTerminateAfterLastWindowClosed: false,
		},
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: "dev.catconnect.app",
			OnSecondInstanceLaunch: func(data application.SecondInstanceData) {
				if !a.handleLaunchArgs(data.Args) {
					a.showWindow()
				}
			},
		},
		OnShutdown: a.shutdown,
	})
	d.app = app

	app.Event.OnApplicationEvent(events.Common.ApplicationLaunchedWithUrl, func(e *application.ApplicationEvent) {
		a.handleURL(e.Context().URL())
	})

	d.setup()
	launchedWithLink := a.handleLaunchArgs(os.Args[1:])

	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		a.startup()
		if !launchedWithLink && !slices.Contains(os.Args[1:], backgroundFlag) {
			a.showWindow()
		}
	})

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
