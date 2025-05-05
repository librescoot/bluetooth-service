package service

// Redis keys
const (
	KeyBatterySlot1      = "battery:0"
	KeyBatterySlot2      = "battery:1"
	KeyVehicle           = "vehicle"
	KeyPowerManager      = "power-manager"
	KeyMileage           = "engine-ecu"
	KeyFirmwareVersion   = "system"
	KeyBLEPairingPin     = "ble"
	KeyBLEStatus         = "ble"
	KeyBLECommand        = "ble"
	KeyCBBattery         = "cb-battery" // Added for clarity
	KeyCBBatteryAlert    = "cb-battery:alert" // For STATUS alerts
	KeyCBBatteryFault    = "cb-battery:fault" // For PROTSTATUS and BATTSTATUS faults

	KeyBLECommandList = "scooter:bluetooth"
)

// Battery state constants
const (
	BatteryStateUnknown = 0
	BatteryStateAsleep  = 1
	BatteryStateIdle    = 2
	BatteryStateActive  = 3
)

// MAX1730X Status bits (Subtype 8)
const (
	MAX1730X_STATUS_CURR_MIN_ALERT = (1 << 2)  // 0x0004
	MAX1730X_STATUS_CURR_MAX_ALERT = (1 << 6)  // 0x0040
	MAX1730X_STATUS_VOLT_MIN_ALERT = (1 << 8)  // 0x0100
	MAX1730X_STATUS_VOLT_MAX_ALERT = (1 << 12) // 0x1000
	MAX1730X_STATUS_TEMP_MIN_ALERT = (1 << 9)  // 0x0200
	MAX1730X_STATUS_TEMP_MAX_ALERT = (1 << 13) // 0x2000
	MAX1730X_STATUS_SOC_MIN_ALERT  = (1 << 10) // 0x0400
	MAX1730X_STATUS_SOC_MAX_ALERT  = (1 << 14) // 0x4000
	// Combined mask for all relevant status alert bits
	CB_BATTERY_STATUS_FILTER = MAX1730X_STATUS_CURR_MIN_ALERT | MAX1730X_STATUS_CURR_MAX_ALERT |
		MAX1730X_STATUS_VOLT_MIN_ALERT | MAX1730X_STATUS_VOLT_MAX_ALERT |
		MAX1730X_STATUS_TEMP_MIN_ALERT | MAX1730X_STATUS_TEMP_MAX_ALERT |
		MAX1730X_STATUS_SOC_MIN_ALERT | MAX1730X_STATUS_SOC_MAX_ALERT
)

// MAX1730X Protection Status bits (Subtype 11)
const (
	MAX1730X_PROTSTATUS_ODCP     = (1 << 2)  // 0x0004
	MAX1730X_PROTSTATUS_UVP      = (1 << 3)  // 0x0008
	MAX1730X_PROTSTATUS_TOOHOTD  = (1 << 4)  // 0x0010
	MAX1730X_PROTSTATUS_DIEHOT   = (1 << 5)  // 0x0020
	MAX1730X_PROTSTATUS_TOOCOLDC = (1 << 12) // 0x1000
	MAX1730X_PROTSTATUS_OVP      = (1 << 11) // 0x0800
	MAX1730X_PROTSTATUS_OCCP     = (1 << 10) // 0x0400
	MAX1730X_PROTSTATUS_QOVFLW   = (1 << 9)  // 0x0200
	MAX1730X_PROTSTATUS_TOOHOTC  = (1 << 14) // 0x4000
	MAX1730X_PROTSTATUS_FULL     = (1 << 13) // 0x2000
	// Combined mask for all relevant protection status fault bits
	CB_BATTERY_PROTECTION_STATUS_FILTER = MAX1730X_PROTSTATUS_ODCP | MAX1730X_PROTSTATUS_UVP |
		MAX1730X_PROTSTATUS_TOOHOTD | MAX1730X_PROTSTATUS_DIEHOT |
		MAX1730X_PROTSTATUS_TOOCOLDC | MAX1730X_PROTSTATUS_OVP |
		MAX1730X_PROTSTATUS_OCCP | MAX1730X_PROTSTATUS_QOVFLW |
		MAX1730X_PROTSTATUS_TOOHOTC | MAX1730X_PROTSTATUS_FULL
)

// MAX1730X Battery Status bits (Subtype 15)
const (
	MAX1730X_BATTSTATUS_CHG_FET_FAIL    = (1 << 12) // 0x1000
	MAX1730X_BATTSTATUS_DISCHG_FET_FAIL = (1 << 11) // 0x0800
	MAX1730X_BATTSTATUS_FET_FAIL_OPEN   = (1 << 10) // 0x0400
	// Combined mask for all relevant battery status fault bits
	CB_BATTERY_BATT_STATUS_FILTER = MAX1730X_BATTSTATUS_CHG_FET_FAIL |
		MAX1730X_BATTSTATUS_DISCHG_FET_FAIL |
		MAX1730X_BATTSTATUS_FET_FAIL_OPEN
) 