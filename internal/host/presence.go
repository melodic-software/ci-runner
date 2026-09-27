package host

import "time"

// inputIdleSince converts GetTickCount64 and LASTINPUTINFO.dwTime, which is the low 32 bits of the
// same millisecond counter, into an idle age. Unsigned 32-bit subtraction stays correct across the
// ~49.7-day wrap of the low word.
func inputIdleSince(nowTicks uint64, lastInputTicks uint32) time.Duration {
	return time.Duration(uint32(nowTicks)-lastInputTicks) * time.Millisecond
}
