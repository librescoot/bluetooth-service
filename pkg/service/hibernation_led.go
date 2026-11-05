package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/librescoot/bluetooth-service/pkg/ble"
	"github.com/redis/go-redis/v9"
)

const (
	alarmLEDSetting          = "alarm.armed-led"
	alarmLEDHibernateSetting = "alarm.armed-led-hibernate"
)

func hibernationLEDEnabled(general, hibernate, alarm string) bool {
	return general == "true" && (hibernate == "" || hibernate == "true") && alarm == "armed"
}

func (s *Service) syncHibernationLED() error {
	// Serialize the snapshot with its write so concurrent watchers cannot send
	// an older decision after a newer one. Firmware resets require a fresh write.
	s.hibernationLEDMu.Lock()
	defer s.hibernationLEDMu.Unlock()
	if s.link == nil || s.link.Caps()&ble.LinkCapHibernationLED == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var general, hibernate, alarm *redis.StringCmd
	_, snapshotErr := s.ipc.Raw().TxPipelined(ctx, func(p redis.Pipeliner) error {
		general = p.HGet(ctx, "settings", alarmLEDSetting)
		hibernate = p.HGet(ctx, "settings", alarmLEDHibernateSetting)
		alarm = p.HGet(ctx, KeyAlarm, "status")
		return nil
	})
	if errors.Is(snapshotErr, redis.Nil) {
		snapshotErr = nil
	}
	value := uint16(0)
	if snapshotErr == nil && hibernationLEDEnabled(general.Val(), hibernate.Val(), alarm.Val()) {
		value = 1
	}
	if err := writeUARTMessage(s.GetUSock(), ble.TypePowerManagement, ble.TypePowerManagementHibernationLED, value); err != nil {
		return fmt.Errorf("send hibernation LED retention: %w", err)
	}
	if snapshotErr != nil {
		return fmt.Errorf("disabled hibernation LED retention after settings read failed: %w", snapshotErr)
	}
	return nil
}
