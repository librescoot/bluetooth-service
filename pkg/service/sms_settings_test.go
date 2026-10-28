package service

import "testing"

func TestSMSForwardSettingValidation(t *testing.T) {
	spec := settingSchema{Type: "string", Format: "sms-forward-destination"}
	for _, tc := range []struct {
		value string
		valid bool
	}{
		{"off", true}, {"+4915112345678", true}, {"+12345678", false}, {"+19009099099", false},
		{"", false}, {"123456789", false}, {"+012345678", false},
		{"+1234567", false}, {"+1234567890123456", false}, {"+49123 45678", false},
	} {
		err := validateSettingValue(spec, tc.value)
		if (err == "") != tc.valid {
			t.Errorf("%q: %s", tc.value, err)
		}
	}
}
