package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/librescoot/bluetooth-service/pkg/logger"
	"github.com/librescoot/bluetooth-service/pkg/redis"
	"github.com/librescoot/bluetooth-service/pkg/service"
	"github.com/librescoot/bluetooth-service/pkg/usock"
)

// Configuration flags
var (
	serialDevice = flag.String("serial", "/dev/ttymxc1", "Serial device path")
	baudRate     = flag.Int("baud", 115200, "Serial baud rate")
	redisAddr    = flag.String("redis-addr", "localhost:6379", "Redis server address")
	redisPass    = flag.String("redis-pass", "", "Redis password")
	redisDB      = flag.Int("redis-db", 0, "Redis database number")
	logLevel     = flag.Int("log-level", int(logger.LogLevelInfo), "Log level (0=none, 1=error, 2=warning, 3=info, 4=debug)")
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

	var stdLogger *log.Logger
	if os.Getenv("INVOCATION_ID") != "" {
		stdLogger = log.New(os.Stdout, "", 0)
	} else {
		stdLogger = log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds|log.Lmsgprefix)
	}

	log := logger.NewLogger(stdLogger, logger.LogLevel(*logLevel))

	log.Infof("Starting MDB Bluetooth Service")
	log.Infof("Serial device: %s", *serialDevice)
	log.Infof("Baud rate: %d", *baudRate)
	log.Infof("Redis address: %s", *redisAddr)

	redisClient, err := redis.New(*redisAddr, *redisPass, *redisDB)
	if err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}
	defer redisClient.Close()
	log.Infof("Connected to Redis")

	svc := service.New(redisClient, log)

	usockHandler := func(payload *usock.Payload) {
		svc.HandleUSockMessage(payload.ID, payload)
	}
	sock, err := usock.New(*serialDevice, *baudRate, usockHandler, log)
	if err != nil {
		svc.SetBLEError(fmt.Sprintf("Failed to open serial port: %v", err))
		log.Fatalf("Failed to connect to nRF52 via USOCK: %v", err)
	}
	svc.SetUSock(sock)

	// Set error handler for serial communication errors
	sock.SetErrorHandler(func(err error) {
		svc.SetBLEError(err.Error())
	})

	defer sock.Close()
	log.Infof("Connected to nRF52 via USOCK")

	// Start the command watcher goroutine
	go svc.WatchRedisCommands()

	// Subscribe to Redis Pub/Sub channels for state updates
	svc.SubscribeToRedisChannels()

	log.Infof("Subscribed to Redis channels")

	// Start health heartbeat before initialization
	svc.StartHealthHeartbeat()

	log.Infof("Initializing communication with nRF52...")
	if err := svc.InitializeNRF52(); err != nil {
		svc.SetBLEError(fmt.Sprintf("Failed to initialize nRF52: %v", err))
		log.Errorf("Error during nRF52 initialization sequence: %v", err)
	} else {
		svc.ClearBLEError()
		log.Infof("nRF52 initialization sequence sent successfully.")
	}

	log.Debugf("Waiting briefly before sending initial state updates...")
	time.Sleep(200 * time.Millisecond)

	log.Infof("Sending initial state updates...")

	// Update vehicle state
	if err := svc.UpdateVehicleState(); err != nil {
		log.Warnf("Warning during initial update: %v", err)
	}
	// Update seatbox lock state
	if err := svc.UpdateSeatboxLock(); err != nil {
		log.Warnf("Warning during initial update: %v", err)
	}
	// Update handlebar lock state
	if err := svc.UpdateHandlebarLock(); err != nil {
		log.Warnf("Warning during initial update: %v", err)
	}
	// Update mileage
	if err := svc.UpdateMileage(); err != nil {
		log.Warnf("Warning during initial update: %v", err)
	}
	// Update firmware version
	if err := svc.UpdateFirmwareVersion(); err != nil {
		log.Warnf("Warning during initial update: %v", err)
	}
	// Update battery states for both slots
	for slot := 0; slot <= 1; slot++ {
		if err := svc.UpdateBatteryPresentStatus(slot); err != nil {
			log.Warnf("Warning during initial update (battery:%d): %v", slot, err)
		}
		if err := svc.UpdateBatteryActiveStatus(slot); err != nil {
			log.Warnf("Warning during initial update (battery:%d): %v", slot, err)
		}
		if err := svc.UpdateBatteryCycleCount(slot); err != nil {
			log.Warnf("Warning during initial update (battery:%d): %v", slot, err)
		}
		if err := svc.UpdateBatteryRemainingCharge(slot); err != nil {
			log.Warnf("Warning during initial update (battery:%d): %v", slot, err)
		}
	}
	// Update power management state
	if err := svc.UpdatePowerManagementState(); err != nil {
		log.Warnf("Warning during initial update: %v", err)
	}
	log.Infof("Initial state updates sent.")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	<-sigCh
	log.Infof("Shutting down...")
	svc.Stop()
	svc.Wait()
}
