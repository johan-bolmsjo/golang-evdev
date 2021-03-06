// +build linux

package evdev

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"github.com/npat-efault/poller"
)

// A Linux input device from which events can be read.
type InputDevice struct {
	Fn string // path to input device (devnode)

	Name string     // device name
	Phys string     // physical topology of device
	File *poller.FD // an open file handle to the input device

	Bustype uint16 // bus type identifier
	Vendor  uint16 // vendor identifier
	Product uint16 // product identifier
	Version uint16 // version identifier

	EvdevVersion int // evdev protocol version

	Capabilities     map[CapabilityType][]CapabilityCode // supported event types and codes.
	CapabilitiesFlat map[int][]int
}

// Open an evdev input device.
func Open(devnode string) (*InputDevice, error) {
	f, err := poller.Open(devnode, poller.O_RO)
	if err != nil {
		return nil, err
	}

	dev := InputDevice{}
	dev.Fn = devnode
	dev.File = f

	if err := dev.set_device_info(); err != nil {
		return nil, fmt.Errorf("read device info: %s", err)
	}
	if err := dev.set_device_capabilities(); err != nil {
		return nil, fmt.Errorf("read device capabilities: %s", err)
	}

	return &dev, nil
}

// Read and return a slice of input events from device.
func (dev *InputDevice) Read() ([]InputEvent, error) {
	events := make([]InputEvent, 16)
	buffer := make([]byte, eventsize*16)

	_, err := dev.File.Read(buffer)
	if err != nil {
		return events, err
	}

	b := bytes.NewBuffer(buffer)
	err = binary.Read(b, binary.LittleEndian, &events)
	if err != nil {
		return events, err
	}

	// remove trailing structures
	for i := range events {
		if events[i].Time.Sec == 0 {
			events = append(events[:i])
			break
		}
	}

	return events, err
}

// Read and return a single input event.
func (dev *InputDevice) ReadOne() (*InputEvent, error) {
	event := InputEvent{}
	buffer := make([]byte, eventsize)

	_, err := dev.File.Read(buffer)
	if err != nil {
		return &event, err
	}

	b := bytes.NewBuffer(buffer)
	err = binary.Read(b, binary.LittleEndian, &event)
	if err != nil {
		return &event, err
	}

	return &event, err
}

// Get a useful description for an input device. Example:
//   InputDevice /dev/input/event3 (fd 3)
//     name Logitech USB Laser Mouse
//     phys usb-0000:00:12.0-2/input0
//     bus 0x3, vendor 0x46d, product 0xc069, version 0x110
//     events EV_KEY 1, EV_SYN 0, EV_REL 2, EV_MSC 4
func (dev *InputDevice) String() string {
	evtypes := make([]string, 0)

	for ev := range dev.Capabilities {
		evtypes = append(evtypes, fmt.Sprintf("%s %d", ev.Name, ev.Type))
	}
	evtypes_s := strings.Join(evtypes, ", ")

	return fmt.Sprintf(
		"InputDevice %s (fd %d)\n"+
			"  name %s\n"+
			"  phys %s\n"+
			"  bus 0x%04x, vendor 0x%04x, product 0x%04x, version 0x%04x\n"+
			"  events %s",
		dev.Fn, dev.File.Sysfd(), dev.Name, dev.Phys, dev.Bustype,
		dev.Vendor, dev.Product, dev.Version, evtypes_s)
}

// Gets the event types and event codes that the input device supports.
func (dev *InputDevice) set_device_capabilities() error {
	// Capabilities is a map of supported event types to lists of
	// events e.g: {1: [272, 273, 274, 275], 2: [0, 1, 6, 8]}
	// capabilities := make(map[int][]int)
	capabilities := make(map[CapabilityType][]CapabilityCode)

	evbits := new([(EV_MAX + 1) / 8]byte)
	codebits := new([(KEY_MAX + 1) / 8]byte)
	// absbits  := new([6]byte)

	if err := dev.File.Lock(); err != nil {
		return err
	}
	defer dev.File.Unlock()
	sysfd := uintptr(dev.File.Sysfd())

	errno := ioctl(sysfd, uintptr(EVIOCGBIT(0, EV_MAX)), unsafe.Pointer(evbits))
	if errno != 0 {
		return errno
	}

	// Build a map of the device's capabilities
	for evtype := 0; evtype < EV_MAX; evtype++ {
		if evbits[evtype/8]&(1<<uint(evtype%8)) != 0 {
			eventcodes := make([]CapabilityCode, 0)

			if errno = ioctl(sysfd, uintptr(EVIOCGBIT(evtype, KEY_MAX)), unsafe.Pointer(codebits)); errno != 0 {
				return errno
			}

			for evcode := 0; evcode < KEY_MAX; evcode++ {
				if codebits[evcode/8]&(1<<uint(evcode%8)) != 0 {
					c := CapabilityCode{evcode, ByEventType[evtype][evcode]}
					eventcodes = append(eventcodes, c)
				}
			}

			// capabilities[EV_KEY] = [KEY_A, KEY_B, KEY_C, ...]
			key := CapabilityType{evtype, EV[evtype]}
			capabilities[key] = eventcodes
		}
	}

	dev.Capabilities = capabilities
	return nil
}

// An all-in-one function for describing an input device.
func (dev *InputDevice) set_device_info() error {
	info := device_info{}

	name := new([MAX_NAME_SIZE]byte)
	phys := new([MAX_NAME_SIZE]byte)

	if err := dev.File.Lock(); err != nil {
		return err
	}
	defer dev.File.Unlock()
	sysfd := uintptr(dev.File.Sysfd())

	errno := ioctl(sysfd, uintptr(EVIOCGID), unsafe.Pointer(&info))
	if errno != 0 {
		return errno
	}

	if errno = ioctl(sysfd, uintptr(EVIOCGNAME), unsafe.Pointer(name)); errno != 0 {
		return errno
	}

	// it's ok if the topology info is not available
	if errno = ioctl(sysfd, uintptr(EVIOCGPHYS), unsafe.Pointer(phys)); errno != 0 {
		return errno
	}

	dev.Name = bytes_to_string(name)
	dev.Phys = bytes_to_string(phys)

	dev.Vendor = info.vendor
	dev.Bustype = info.bustype
	dev.Product = info.product
	dev.Version = info.version

	ev_version := new(int)

	if errno = ioctl(sysfd, uintptr(EVIOCGVERSION), unsafe.Pointer(ev_version)); errno != 0 {
		return errno
	}

	dev.EvdevVersion = *ev_version

	return nil
}

// Get repeat rate and delay.
// Repeat rate is in characters per second. Delay is the amount of time in
// milliseconds that a key must be depressed before it will start to repeat.
func (dev *InputDevice) GetRepeatRate() (repeat, delay uint, err error) {
	if err = dev.File.Lock(); err == nil {
		return
	}
	defer dev.File.Unlock()
	sysfd := uintptr(dev.File.Sysfd())

	var t [2]uint
	if errno := ioctl(sysfd, uintptr(EVIOCGREP), unsafe.Pointer(&t)); errno != 0 {
		err = errno
		return
	}

	repeat, delay = t[0], t[1]
	return
}

// Set repeat rate and delay.
func (dev *InputDevice) SetRepeatRate(repeat, delay uint) error {
	if err := dev.File.Lock(); err == nil {
		return err
	}
	defer dev.File.Unlock()
	sysfd := uintptr(dev.File.Sysfd())

	t := [2]uint{repeat, delay}
	if errno := ioctl(sysfd, uintptr(EVIOCSREP), unsafe.Pointer(&t)); errno != 0 {
		return errno
	}
	return nil
}

// Enable exclusive listening of the device. This is useful if you want to
// capture all events from a device, like a macro pad, keyboard, or gaming
// mouse.
func (dev *InputDevice) Grab() error {
	if err := dev.File.Lock(); err != nil {
		return err
	}
	defer dev.File.Unlock()
	sysfd := uintptr(dev.File.Sysfd())

	// IOCTL use pointer value itself as indication to grab or release device.
	var anyPtr int
	if errno := ioctl(sysfd, uintptr(EVIOCGRAB), unsafe.Pointer(&anyPtr)); errno != 0 {
		return errno
	}
	return nil
}

// Disable exclusive listening of the device.
func (dev *InputDevice) Release() error {
	if err := dev.File.Lock(); err != nil {
		return err
	}
	defer dev.File.Unlock()
	sysfd := uintptr(dev.File.Sysfd())

	if errno := ioctl(sysfd, uintptr(EVIOCGRAB), unsafe.Pointer(nil)); errno != 0 {
		return errno
	}
	return nil
}

type CapabilityType struct {
	Type int
	Name string
}

type CapabilityCode struct {
	Code int
	Name string
}

type AbsInfo struct {
	value      int32
	minimum    int32
	maximum    int32
	fuzz       int32
	flat       int32
	resolution int32
}

// Corresponds to the input_id struct.
type device_info struct {
	bustype, vendor, product, version uint16
}

// Return the keys of a map as a slice (dict.keys())
func keys(cap *map[int][]int) []int {
	slice := make([]int, 0)

	for key := range *cap {
		slice = append(slice, key)
	}

	return slice
}

// Determine if a path exist and is a character input device.
func IsInputDevice(path string) bool {
	fi, err := os.Stat(path)

	if os.IsNotExist(err) {
		return false
	}

	m := fi.Mode()
	if m&os.ModeCharDevice == 0 {
		return false
	}

	return true
}

// Return a list of accessible input device names matched by
// deviceglob (default '/dev/input/event*').
func ListInputDevicePaths(device_glob string) ([]string, error) {
	paths, err := filepath.Glob(device_glob)

	if err != nil {
		return nil, err
	}

	devices := make([]string, 0)
	for _, path := range paths {
		if IsInputDevice(path) {
			devices = append(devices, path)
		}
	}

	return devices, nil
}

// Return a list of accessible input devices matched by deviceglob
// (default '/dev/input/event/*').
func ListInputDevices(device_glob_arg ...string) ([]*InputDevice, error) {
	device_glob := "/dev/input/event*"
	if len(device_glob_arg) > 0 {
		device_glob = device_glob_arg[0]
	}

	fns, _ := ListInputDevicePaths(device_glob)
	devices := make([]*InputDevice, 0)

	for i := range fns {
		dev, err := Open(fns[i])
		if err == nil {
			devices = append(devices, dev)
		}
	}

	return devices, nil
}

func bytes_to_string(b *[MAX_NAME_SIZE]byte) string {
	idx := bytes.IndexByte(b[:], 0)
	return string(b[:idx])
}
