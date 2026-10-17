package runtimetune

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/baditaflorin/go-common/env"
)

// The variables ConfigFromEnv reads, declared in the env registry so that they
// are listed, validated at start-up and shown (redacted) by the debug endpoint.
// The Check functions state the same rules the readers apply; env_test.go keeps
// the two in step.
var (
	envKillSwitch = env.Declare(env.Var{
		Name: EnvKillSwitch, Kind: env.KindString, Default: "enabled",
		Doc:   "Set to a falsey value to disable all runtime tuning. Anything unrecognised also disables it, so a typo here turns tuning off.",
		Check: checkKillSwitch,
	})
	envMemLimitRatio = env.Declare(env.Var{
		Name: EnvMemLimitRatio, Kind: env.KindFloat, Default: fmt.Sprint(DefaultMemLimitRatio),
		Doc:   "Fraction of the cgroup memory limit used as GOMEMLIMIT, strictly between 0 and 1.",
		Check: checkRatio,
	})
	envMinHeapBytes = env.Declare(env.Var{
		Name: EnvMinHeapBytes, Kind: env.KindInt, Default: fmt.Sprint(DefaultMinHeapTarget),
		Doc:   "Minimum heap target in bytes that the GOGC floor aims for; 0 disables the floor and leaves GOGC untouched.",
		Check: checkNonNegativeInt,
	})
	envBaseGOGC = env.Declare(env.Var{
		Name: EnvBaseGOGC, Kind: env.KindInt, Default: fmt.Sprint(DefaultBaseGOGC),
		Doc:   "GOGC used once the live heap is at or above the minimum heap target; positive.",
		Check: checkPositiveInt,
	})
	envMaxGOGC = env.Declare(env.Var{
		Name: EnvMaxGOGC, Kind: env.KindInt, Default: fmt.Sprint(DefaultMaxGOGC),
		Doc:   "Upper bound on the GOGC the floor logic may set; positive.",
		Check: checkPositiveInt,
	})
	envInterval = env.Declare(env.Var{
		Name: EnvInterval, Kind: env.KindDuration, Default: DefaultInterval.String(),
		Doc:   "How often the GOGC floor is recomputed; a positive duration such as 5s.",
		Check: checkPositiveDuration,
	})
)

func checkKillSwitch(v string) error {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on", "enabled", "0", "false", "no", "off", "disabled":
		return nil
	}
	return fmt.Errorf("not recognised, so tuning would be disabled (use enabled or disabled)")
}

func checkRatio(v string) error {
	if f, err := strconv.ParseFloat(v, 64); err != nil || f <= 0 || f >= 1 {
		return fmt.Errorf("not a ratio strictly between 0 and 1")
	}
	return nil
}

func checkNonNegativeInt(v string) error {
	if n, err := strconv.ParseInt(v, 10, 64); err != nil || n < 0 {
		return fmt.Errorf("not a non-negative integer")
	}
	return nil
}

func checkPositiveInt(v string) error {
	if n, err := strconv.Atoi(v); err != nil || n <= 0 {
		return fmt.Errorf("not a positive integer")
	}
	return nil
}

func checkPositiveDuration(v string) error {
	if d, err := time.ParseDuration(v); err != nil || d <= 0 {
		return fmt.Errorf("not a positive duration")
	}
	return nil
}
