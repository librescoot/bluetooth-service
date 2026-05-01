package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	ipc "github.com/librescoot/redis-ipc"

	"github.com/librescoot/bluetooth-service/pkg/logger"
	"github.com/librescoot/bluetooth-service/pkg/service"
	"github.com/librescoot/bluetooth-service/pkg/usock"
)

var version = "dev"

// Configuration flags
var (
	serialDevice = flag.String("serial", "/dev/ttymxc1", "Serial device path")
	baudRate     = flag.Int("baud", 115200, "Serial baud rate")
	redisAddr    = flag.String("redis-addr", "localhost:6379", "Redis server address")
	redisPass    = flag.String("redis-pass", "", "Redis password")
	redisDB      = flag.Int("redis-db", 0, "Redis database number")
	logLevel     = flag.Int("log-level", int(logger.LogLevelInfo), "Log level (0=none, 1=error, 2=warning, 3=info, 4=debug)")
	showVersion  = flag.Bool("version", false, "Print version and exit")
	firmwareDir  = flag.String("firmware-dir", service.DefaultFirmwareDir, "Directory containing firmware files")
	autoUpdate   = flag.Bool("auto-update", true, "Automatically update firmware on startup if newer version available")
)

// Redis keys
const (
	KeyBatterySlot1    = "battery:0"
	KeyBatterySlot2    = "battery:1"
	KeyVehicle         = "vehicle"
	KeyPowerManager    = "power-manager"
	KeyMileage         = "engine-ecu"
	KeyFirmwareVersion = "system"
	KeyBLEPairingPin   = "ble"
	KeyBLEStatus       = "ble"
	KeyBLECommand      = "ble"
)

// Battery state constants
const (
	BatteryStateUnknown = 0
	BatteryStateAsleep  = 1
	BatteryStateIdle    = 2
	BatteryStateActive  = 3
)

func main() {
	flag.Parse()

	if *showVersion {
		fmt.Printf("bluetooth-service %s\n", version)
		return
	}

	var stdLogger *log.Logger
	if os.Getenv("INVOCATION_ID") != "" {
		stdLogger = log.New(os.Stdout, "", 0)
	} else {
		stdLogger = log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds|log.Lmsgprefix)
	}

	log := logger.NewLogger(stdLogger, logger.LogLevel(*logLevel))

	log.Infof("librescoot-bluetooth %s starting", version)
	log.Infof("Serial device: %s", *serialDevice)
	log.Infof("Baud rate: %d", *baudRate)
	log.Infof("Redis address: %s", *redisAddr)

	ipcClient, err := ipc.New(
		ipc.WithURL(*redisAddr),
		ipc.WithCodec(ipc.StringCodec{}),
	)
	if err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}
	defer ipcClient.Close()
	log.Infof("Connected to Redis")

	svc := service.New(ipcClient, log)

	usockHandler := func(payload *usock.Payload) {
		svc.HandleUSockMessage(payload.ID, payload)
	}
	usockErrHandler := func(err error) {
		log.Errorf("Serial communication error: %v", err)
		svc.SetFault(service.FaultSerialPort)
	}
	sock, err := usock.New(*serialDevice, *baudRate, usockHandler, log)
	if err != nil {
		log.Fatalf("Failed to connect to nRF52 via USOCK: %v", err)
	}
	svc.SetUSock(sock)

	svc.SetSerialConfig(*serialDevice, *baudRate, usockHandler, usockErrHandler)
	svc.SetAutoUpdate(*autoUpdate)

	// Create and configure firmware updater
	fwConfig := service.FirmwareUpdateConfig{
		FirmwareDir:     *firmwareDir,
		NRFUpdateScript: service.DefaultNRFUpdateScript,
		SerialDevice:    *serialDevice,
	}
	fwUpdater := service.NewFirmwareUpdater(fwConfig, log, ipcClient, svc)
	svc.SetFirmwareUpdater(fwUpdater)

	log.Infof("Firmware update: auto-update=%v, firmware-dir=%s", *autoUpdate, *firmwareDir)

	sock.SetErrorHandler(usockErrHandler)

	defer sock.Close()
	svc.ClearFault(service.FaultSerialPort)
	log.Infof("Connected to nRF52 via USOCK")

	// Start the command watcher goroutine
	go svc.WatchRedisCommands()

	// Subscribe to Redis Pub/Sub channels for state updates
	svc.SubscribeToRedisChannels()

	log.Infof("Subscribed to Redis channels")

	log.Infof("Initializing communication with nRF52...")
	if err := svc.InitializeNRF52(); err != nil {
		svc.SetFault(service.FaultNRFInit)
		log.Errorf("Error during nRF52 initialization sequence: %v", err)
	} else {
		svc.ClearFault(service.FaultNRFInit)
		log.Infof("nRF52 initialization sequence sent successfully.")
	}

	log.Infof("Initial state loaded via StartWithSync callbacks")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	<-sigCh
	log.Infof("Shutting down...")
	svc.ShutdownNRF52()
	svc.Stop()
	svc.Wait()
}
