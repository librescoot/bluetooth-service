package ble

// MessageType represents the type of BLE message
type MessageType uint16

const (
	// Message types
	TypeDataStream        MessageType = 0x00C0 // BLE_SCOOTER_SERVICE_DATA_STREAM
	TypeBLEParam          MessageType = 0xA080 // BLE_SCOOTER_SERVICE_BLE_PARAM
	TypeBattery           MessageType = 0x00E0 // BLE_SCOOTER_SERVICE_BATTERY
	TypeVehicleState      MessageType = 0x0020 // BLE_SCOOTER_SERVICE_SCOOTER_STATE
	TypeScooterInfo       MessageType = 0xA040 // BLE_SCOOTER_SERVICE_SCOOTER_INFO
	TypePowerManagement   MessageType = 0x0800 // BLE_SCOOTER_SERVICE_POWER_MANAGEMENT
	TypeBLEPairingPinDisplay MessageType = 0xA080 + 2 // BLE_SCOOTER_SERVICE_BLE_PAIRING_PIN_DISPLAY
	TypeBLEPairingPinRemove  MessageType = 0xA080 + 3 // BLE_SCOOTER_SERVICE_BLE_PAIRING_PIN_REMOVE
	TypeBLEStatus         MessageType = 0xA080 + 4 // BLE_SCOOTER_SERVICE_BLE_STATUS
	TypeBLECommand        MessageType = 0xAA00 // BLE_SCOOTER_SERVICE_BLE_COMMANDS
	TypeBLEDebug          MessageType = 0xA020 // BLE_SCOOTER_SERVICE_DEBUG
	TypeBLEReset          MessageType = 0xA020 + 1 // BLE_SCOOTER_SERVICE_DEBUG_RESET_INFO
	TypeBLEVersion        MessageType = 0xA000 // BLE_SCOOTER_SERVICE_VERSION
	TypeAuxBattery        MessageType = 0x0040 // BLE_SCOOTER_SERVICE_AUX_BATTERY
	TypeBatteryInfo       MessageType = 0x0060 // BLE_SCOOTER_SERVICE_BATTERY_INFO (Assumed)
	TypePowerMux          MessageType = 0x0100 // BLE_SCOOTER_SERVICE_POWER_MUX_STATE
)

// SubType represents the sub-type of a message
type SubType uint16

const (
	// Data stream sub-types
	TypeDataStreamEnable SubType = 1 // BLE_SCOOTER_SERVICE_DATA_STREAM_ENABLE
	TypeDataStreamSync   SubType = 2 // BLE_SCOOTER_SERVICE_DATA_STREAM_SYNC

	// BLE parameter sub-types
	TypeBLEParamMACAddress SubType = 1 // BLE_SCOOTER_SERVICE_BLE_PARAM_MAC_ADDRESS
	TypeBLEParamDeleteBonds SubType = 2 // BLE_SCOOTER_SERVICE_BLE_PARAM_DELETE_BONDS
	TypeBLEParamAdvertising SubType = 3 // BLE_SCOOTER_SERVICE_BLE_PARAM_ADVERTISING
	TypeBLEParamData        SubType = 24 // 0x18 - Custom data parameter

	// Battery sub-types (Slot 1)
	TypeBatterySlot1Base           SubType = 0x00E0 // Base for slot 1
	TypeBatterySlot1State          SubType = 0x00E2 // Relative subtype 2
	TypeBatterySlot1Presence       SubType = 0x00E3 // Relative subtype 3
	TypeBatterySlot1CycleCount     SubType = 0x00E6 // Relative subtype 6
	TypeBatterySlot1Charge         SubType = 0x00E9 // Relative subtype 9
	// Add other slot 1 metrics if needed
	// TypeBatterySlot1Voltage       SubType = 7
	// TypeBatterySlot1Current       SubType = 8
	// TypeBatterySlot1FullCharge    SubType = 10
	// TypeBatterySlot1Temperature   SubType = 11
	// TypeBatterySlot1Health        SubType = 12
	// TypeBatterySlot1FaultCode     SubType = 13
	// TypeBatterySlot1SerialNumber  SubType = 4
	// TypeBatterySlot1ManufacDate   SubType = 5

	// Battery sub-types (Slot 2)
	TypeBatterySlot2Base           SubType = 0x00EC // Base for slot 2
	TypeBatterySlot2State          SubType = 0x00EE // Relative subtype 14 (0xEC + 2)
	TypeBatterySlot2Presence       SubType = 0x00EF // Relative subtype 15 (0xEC + 3)
	TypeBatterySlot2CycleCount     SubType = 0x00F2 // Relative subtype 18 (0xEC + 6)
	TypeBatterySlot2Charge         SubType = 0x00F5 // Relative subtype 21 (0xEC + 9)
	// Add other slot 2 metrics if needed
	// TypeBatterySlot2Voltage       SubType = 19
	// TypeBatterySlot2Current       SubType = 20
	// TypeBatterySlot2FullCharge    SubType = 22
	// TypeBatterySlot2Temperature   SubType = 23
	// TypeBatterySlot2Health        SubType = 24
	// TypeBatterySlot2FaultCode     SubType = 25
	// TypeBatterySlot2SerialNumber  SubType = 16
	// TypeBatterySlot2ManufacDate   SubType = 17

	// Vehicle state sub-types
	TypeVehicleStateState     SubType = 1 // BLE_SCOOTER_SERVICE_SCOOTER_STATE_STATE
	TypeVehicleStateSeatbox   SubType = 2 // BLE_SCOOTER_SERVICE_SCOOTER_STATE_SEATBOX
	TypeVehicleStateHandlebar SubType = 3 // BLE_SCOOTER_SERVICE_SCOOTER_STATE_HANDLEBAR

	// Scooter info sub-types
	TypeSoftwareVersion SubType = 1 // BLE_SCOOTER_SERVICE_SOFTWARE_VERSION
	TypeMileage         SubType = 2 // BLE_SCOOTER_SERVICE_MILEAGE

	// Power management sub-types
	TypePowerManagementState        SubType = 1 // BLE_SCOOTER_SERVICE_POWER_MANAGEMENT_STATE
	TypePowerManagementPowerRequest SubType = 2 // BLE_SCOOTER_SERVICE_POWER_MANAGEMENT_POWER_REQUEST
	
	// BLE debug sub-types
	TypeBLEDebugResetAck SubType = 3 // BLE_SCOOTER_SERVICE_DEBUG_RESET_ACK
	
	// BLE version sub-types
	TypeBLEVersionString SubType = 1 // BLE_SCOOTER_SERVICE_VERSION_STRING
	TypeBLEVersionRequest SubType = 2 // BLE_SCOOTER_SERVICE_VERSION_REQUEST
	
	// Aux battery sub-types
	TypeAuxBatteryVoltage       SubType = 1 // BLE_SCOOTER_SERVICE_AUX_BATTERY_VOLTAGE
	TypeAuxBatteryCharge        SubType = 4 // BLE_SCOOTER_SERVICE_AUX_BATTERY_CHARGE
	TypeAuxBatteryChargerStatus SubType = 3 // BLE_SCOOTER_SERVICE_AUX_BATTERY_CHARGER_STATUS

	// Battery Info (CB Battery) sub-types based on ble_service_cb_battery_t
	TypeBatteryInfoCharge            SubType = 1  // BLE_SCOOTER_SERVICE_CB_BATTERY_CHARGE
	TypeBatteryInfoCurrent           SubType = 2  // BLE_SCOOTER_SERVICE_CB_BATTERY_CURRENT
	TypeBatteryInfoRemCapacity       SubType = 3  // BLE_SCOOTER_SERVICE_CB_BATTERY_REMAINING_CAPACITY
	TypeBatteryInfoFullCapacity      SubType = 4  // BLE_SCOOTER_SERVICE_CB_BATTERY_FULL_CAPACITY
	TypeBatteryInfoCellVoltage       SubType = 5  // BLE_SCOOTER_SERVICE_CB_BATTERY_CELL_VOLTAGE
	TypeBatteryInfoTemp              SubType = 6  // BLE_SCOOTER_SERVICE_CB_BATTERY_TEMPERATURE
	TypeBatteryInfoCycleCount        SubType = 7  // BLE_SCOOTER_SERVICE_CB_BATTERY_CYCLE_COUNT
	TypeBatteryInfoStatus            SubType = 8  // BLE_SCOOTER_SERVICE_CB_BATTERY_STATUS
	TypeBatteryInfoTTE               SubType = 9  // BLE_SCOOTER_SERVICE_CB_BATTERY_TTE (Time-To-Empty)
	TypeBatteryInfoTTF               SubType = 10 // BLE_SCOOTER_SERVICE_CB_BATTERY_TTF (Time-To-Full)
	TypeBatteryInfoProtectionStatus  SubType = 11 // BLE_SCOOTER_SERVICE_CB_BATTERY_PROTECTION_STATUS
	TypeBatteryInfoSOH               SubType = 12 // BLE_SCOOTER_SERVICE_CB_BATTERY_SOH (State Of Health)
	TypeBatteryInfoUniqueID          SubType = 13 // BLE_SCOOTER_SERVICE_CB_BATTERY_UNIQUE_ID
	TypeBatteryInfoSerialNumber      SubType = 14 // BLE_SCOOTER_SERVICE_CB_BATTERY_SERIAL_NO
	TypeBatteryInfoBattStatus        SubType = 15 // BLE_SCOOTER_SERVICE_CB_BATTERY_BATT_STATUS
	TypeBatteryInfoPartNo            SubType = 16 // BLE_SCOOTER_SERVICE_CB_BATTERY_PART_NO
	TypeBatteryInfoPresent           SubType = 17 // BLE_SCOOTER_SERVICE_CB_BATTERY_PRESENT
	TypeBatteryInfoChargeStatus      SubType = 18 // BLE_SCOOTER_SERVICE_CB_BATTERY_CHARGE_STATUS
	// Note: Subtypes used here are relative to TypeBatteryInfo (0x0060)
)

// BLECommand represents BLE control commands
type BLECommand uint8

const (
	BLECommandAdvStartWithWhitelist BLECommand = 1  // BLE_SCOOTER_SERVICE_BLE_COMMANDS_ADV_START_WITH_WHITELISTING
	BLECommandAdvRestartNoWhitelist BLECommand = 2  // BLE_SCOOTER_SERVICE_BLE_COMMANDS_ADV_RESTART_NO_WHITELISTING
	BLECommandAdvStop               BLECommand = 3  // BLE_SCOOTER_SERVICE_BLE_COMMANDS_ADV_STOP
	BLECommandDeleteBond            BLECommand = 4  // BLE_SCOOTER_SERVICE_BLE_COMMANDS_DELETE_BOND
	BLECommandDeleteAllBonds        BLECommand = 5  // BLE_SCOOTER_SERVICE_BLE_COMMANDS_DELETE_ALL_BONDS
)

// BatterySlot represents a battery slot number
type BatterySlot uint8

const (
	BatterySlot1 BatterySlot = 0  // First battery slot (index 0)
	BatterySlot2 BatterySlot = 1  // Second battery slot (index 1)
)

// Message represents a BLE message
type Message struct {
	Type    MessageType
	SubType SubType
	Slot    BatterySlot // Used for battery-related messages
	Payload []byte
}

// BLECharacteristic represents a BLE characteristic with its properties
type BLECharacteristic struct {
	UUID        string
	Name        string
	IsReadable  bool
	IsWritable  bool
	IsNotifying bool
}

// Define BLE characteristics
var (
	CharBatteryStatus = BLECharacteristic{
		UUID:        "2A19",
		Name:        "Battery Status",
		IsReadable:  true,
		IsWritable:  false,
		IsNotifying: true,
	}

	CharVehicleState = BLECharacteristic{
		UUID:        "2A57",
		Name:        "Vehicle State",
		IsReadable:  true,
		IsWritable:  false,
		IsNotifying: true,
	}

	CharSeatboxLock = BLECharacteristic{
		UUID:        "2A58",
		Name:        "Seatbox Lock",
		IsReadable:  true,
		IsWritable:  true,
		IsNotifying: true,
	}

	CharHandlebarLock = BLECharacteristic{
		UUID:        "2A59",
		Name:        "Handlebar Lock",
		IsReadable:  true,
		IsWritable:  false,
		IsNotifying: true,
	}

	CharPowerManagement = BLECharacteristic{
		UUID:        "2A5A",
		Name:        "Power Management",
		IsReadable:  true,
		IsWritable:  true,
		IsNotifying: true,
	}

	CharBLEPairingPinDisplay = BLECharacteristic{
		UUID:        "A082",
		Name:        "BLE Pairing PIN Display",
		IsReadable:  true,
		IsWritable:  false,
		IsNotifying: true,
	}

	CharBLEPairingPinRemove = BLECharacteristic{
		UUID:        "A083",
		Name:        "BLE Pairing PIN Remove",
		IsReadable:  false,
		IsWritable:  true,
		IsNotifying: false,
	}

	CharBLEStatus = BLECharacteristic{
		UUID:        "A084",
		Name:        "BLE Status",
		IsReadable:  true,
		IsWritable:  false,
		IsNotifying: true,
	}

	CharBLECommand = BLECharacteristic{
		UUID:        "AA00",
		Name:        "BLE Command",
		IsReadable:  false,
		IsWritable:  true,
		IsNotifying: false,
	}
) 