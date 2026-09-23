package service

import "testing"

func TestReadNavPlanTreatsMissingFieldsAsEmpty(t *testing.T) {
	s, _ := newVersionPushService(t)

	stops, step, err := s.readNavPlan()
	if err != nil {
		t.Fatalf("readNavPlan() error = %v", err)
	}
	if len(stops) != 0 || step != 0 {
		t.Fatalf("readNavPlan() = (%v, %d), want empty plan", stops, step)
	}
}

func TestReadNavPlanAllowsMissingCurrentStep(t *testing.T) {
	s, mr := newVersionPushService(t)
	mr.HSet(KeyNavigation, "waypoints", `[{"lat":52.5,"lon":13.4,"label":"Berlin"}]`)

	stops, step, err := s.readNavPlan()
	if err != nil {
		t.Fatalf("readNavPlan() error = %v", err)
	}
	if len(stops) != 1 || stops[0].Label != "Berlin" || step != 0 {
		t.Fatalf("readNavPlan() = (%v, %d), want one stop at step zero", stops, step)
	}
}
