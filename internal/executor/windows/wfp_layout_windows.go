//go:build windows && (amd64 || arm64)

package windows

import "unsafe"

// The WFP structs must match the Windows SDK's layout on 64-bit Windows
// (checked against fwpmtypes.h for x86_64 and aarch64). Each pair of
// constants overflows, and the build fails, unless the two sides are equal.
const (
	_ = unsafe.Sizeof(fwpmFilter0{}) - 200
	_ = 200 - unsafe.Sizeof(fwpmFilter0{})
	_ = unsafe.Offsetof(fwpmFilter0{}.Action) - 128
	_ = 128 - unsafe.Offsetof(fwpmFilter0{}.Action)
	_ = unsafe.Offsetof(fwpmFilter0{}.ProviderContextKey) - 152
	_ = 152 - unsafe.Offsetof(fwpmFilter0{}.ProviderContextKey)
	_ = unsafe.Offsetof(fwpmFilter0{}.EffectiveWeight) - 184
	_ = 184 - unsafe.Offsetof(fwpmFilter0{}.EffectiveWeight)
	_ = unsafe.Offsetof(fwpmFilter0{}.FilterCondition) - 120
	_ = 120 - unsafe.Offsetof(fwpmFilter0{}.FilterCondition)
	_ = unsafe.Sizeof(fwpmFilterCondition0{}) - 40
	_ = 40 - unsafe.Sizeof(fwpmFilterCondition0{})
	_ = unsafe.Offsetof(fwpmFilterCondition0{}.Value) - 24
	_ = 24 - unsafe.Offsetof(fwpmFilterCondition0{}.Value)
	_ = unsafe.Sizeof(fwpmSession0{}) - 72
	_ = 72 - unsafe.Sizeof(fwpmSession0{})
	_ = unsafe.Offsetof(fwpmSession0{}.TxnWaitTimeoutInMSec) - 36
	_ = 36 - unsafe.Offsetof(fwpmSession0{}.TxnWaitTimeoutInMSec)
	_ = unsafe.Sizeof(fwpmProvider0{}) - 64
	_ = 64 - unsafe.Sizeof(fwpmProvider0{})
	_ = unsafe.Offsetof(fwpmProvider0{}.ServiceName) - 56
	_ = 56 - unsafe.Offsetof(fwpmProvider0{}.ServiceName)
	_ = unsafe.Sizeof(fwpmSublayer0{}) - 72
	_ = 72 - unsafe.Sizeof(fwpmSublayer0{})
	_ = unsafe.Offsetof(fwpmSublayer0{}.Weight) - 64
	_ = 64 - unsafe.Offsetof(fwpmSublayer0{}.Weight)
	_ = unsafe.Sizeof(fwpV4AddrAndMask{}) - 8
	_ = 8 - unsafe.Sizeof(fwpV4AddrAndMask{})
	_ = unsafe.Sizeof(fwpV6AddrAndMask{}) - 17
	_ = 17 - unsafe.Sizeof(fwpV6AddrAndMask{})
	_ = unsafe.Offsetof(fwpV6AddrAndMask{}.PrefixLength) - 16
	_ = 16 - unsafe.Offsetof(fwpV6AddrAndMask{}.PrefixLength)
)
