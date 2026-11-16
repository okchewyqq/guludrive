package workspace

import (
	"time"

	"github.com/egoist/mygo/ui"
)

var (
	controlAnimation  = ui.Animation{Duration: 160 * time.Millisecond, Ease: ui.EaseOut}
	pageAnimation     = ui.Animation{Duration: 180 * time.Millisecond, Ease: ui.EaseOut}
	dialogAnimation   = ui.Animation{Duration: 200 * time.Millisecond, Ease: ui.EaseOut}
	controlTransition = ui.ElementTransition{Colors: true, Animation: &controlAnimation}
	pageTransition    = ui.ElementTransition{
		Colors: true, Animation: &pageAnimation, Exit: &ui.Motion{Y: -4},
	}
	dialogTransition = ui.ElementTransition{
		Colors: true, Animation: &dialogAnimation, Exit: &ui.Motion{Y: 8, Scale: .985},
	}
	shadeTransition = ui.ElementTransition{
		Colors: true, Animation: &dialogAnimation, Exit: &ui.Motion{},
	}
)

// Seed one frame, then let MyGo paint the pose without rebuilding the view.
// Reduced motion is handled by MyGo: positions settle at once, fades remain.
func enterPose(c *ui.Context, e ui.Element, from ui.Pose, animation ui.Animation) {
	entered := ui.Local(e, "entered", func() bool { return false })
	pose := ui.Pose{}
	if !*entered {
		pose, *entered = from, true
		c.Invalidate()
	}
	e.AnimatePose(pose, animation)
}

func animateControl(e ui.Element, disabled bool) ui.Element {
	pose := ui.Pose{}
	if !disabled && e.Pressed() {
		pose.Scale = .98
	}
	return e.Transition(controlTransition).AnimatePose(pose, controlAnimation)
}
