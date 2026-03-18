package ublk

import "time"

const (
	// queueShutdownGracePeriod gives STOP_DEV time to abort pending queue work
	// before Delete falls back to interrupting the queue rings.
	queueShutdownGracePeriod = 2 * time.Second
)

type deviceHooks struct {
	startControl       func(*Device) error
	stopControl        func(*Device) error
	stopControlWait    func(*Device) error
	deleteControl      func(*Device) error
	closeChar          func(*Device) error
	closeCtrlRing      func(*Device) error
	closeCtrlFile      func(*Device) error
	waitServe          func(*Device, time.Duration) bool
	interruptIORings   func(*Device)
	applyQueueAffinity func(*Device, uint16)
}

func (d *Device) startControl() error {
	if d.hooks != nil && d.hooks.startControl != nil {
		return d.hooks.startControl(d)
	}
	return d.ctrlStartDev()
}

func (d *Device) stopControl() error {
	if d.hooks != nil && d.hooks.stopControl != nil {
		return d.hooks.stopControl(d)
	}
	return d.ctrlStopDev()
}

func (d *Device) stopControlWait() error {
	if d.hooks != nil && d.hooks.stopControlWait != nil {
		return d.hooks.stopControlWait(d)
	}
	return d.ctrlStopDevWait()
}

func (d *Device) deleteControl() error {
	if d.hooks != nil && d.hooks.deleteControl != nil {
		return d.hooks.deleteControl(d)
	}
	return d.ctrlDelDev()
}

func (d *Device) closeCharDevice() error {
	if d.hooks != nil && d.hooks.closeChar != nil {
		return d.hooks.closeChar(d)
	}
	if d.charFile == nil {
		return nil
	}
	err := d.charFile.Close()
	d.charFile = nil
	return err
}

func (d *Device) closeControlRing() error {
	if d.hooks != nil && d.hooks.closeCtrlRing != nil {
		return d.hooks.closeCtrlRing(d)
	}
	if d.ctrlRing == nil {
		return nil
	}
	return d.ctrlRing.Close()
}

func (d *Device) closeControlFile() error {
	if d.hooks != nil && d.hooks.closeCtrlFile != nil {
		return d.hooks.closeCtrlFile(d)
	}
	if d.ctrlFile == nil {
		return nil
	}
	return d.ctrlFile.Close()
}

func (d *Device) waitServeWithTimeout(timeout time.Duration) bool {
	if d.hooks != nil && d.hooks.waitServe != nil {
		return d.hooks.waitServe(d, timeout)
	}
	return d.waitServe(timeout)
}

func (d *Device) interruptQueueRings() {
	if d.hooks != nil && d.hooks.interruptIORings != nil {
		d.hooks.interruptIORings(d)
		return
	}
	d.ioRingsMu.Lock()
	for _, ring := range d.ioRings {
		if ring != nil {
			ring.Interrupt()
		}
	}
	d.ioRingsMu.Unlock()
}

func (d *Device) applyQueueAffinity(qid uint16) {
	if d.hooks != nil && d.hooks.applyQueueAffinity != nil {
		d.hooks.applyQueueAffinity(d, qid)
		return
	}
	d.setQueueAffinity(qid)
}
