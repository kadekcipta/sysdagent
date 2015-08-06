package main

import (
	"bytes"
	"fmt"
	"goutils/slackconnect"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/godbus/dbus"
)

const (
	envServices   = "X_SYSD_SERVICES"
	envWebhookUri = "X_SYSD_WEBHOOK_URI"

	propertiesChanged = "org.freedesktop.DBus.Properties.PropertiesChanged"
	propGet           = "org.freedesktop.DBus.Properties.Get"
)

type serviceStatus struct {
	ID                     string
	Name                   string
	Path                   dbus.ObjectPath
	ActiveState            string
	ActiveEnterTimestamp   uint64
	ActiveExitTimestamp    uint64
	InactiveEnterTimestamp uint64
	InactiveExitTimestamp  uint64
}

func formatTime(us uint64) string {
	return time.Unix(0, 1000*int64(us)).Format(time.RFC1123Z)
}

func (s serviceStatus) String() string {
	var buf bytes.Buffer

	buf.WriteString(fmt.Sprintf("ID: %s\n", s.ID))
	buf.WriteString(fmt.Sprintf("Name: %s\n", s.Name))
	buf.WriteString(fmt.Sprintf("State: %s\n", s.ActiveState))
	//buf.WriteString(fmt.Sprintf("ActiveEnterTime: %s\n", formatTime(s.ActiveEnterTimestamp)))
	//buf.WriteString(fmt.Sprintf("ActiveExitTime: %s\n", formatTime(s.ActiveExitTimestamp)))
	//buf.WriteString(fmt.Sprintf("InactiveEnterTime: %s\n", formatTime(s.InactiveEnterTimestamp)))
	//buf.WriteString(fmt.Sprintf("InactiveExitTime: %s\n", formatTime(s.InactiveExitTimestamp)))

	return buf.String()
}

type unitMonitor struct {
	name    string
	done    chan struct{}
	path    dbus.ObjectPath
	chanPub chan serviceStatus
}

func (m *unitMonitor) getUnitPath() dbus.ObjectPath {

	var ret dbus.ObjectPath

	c, err := dbus.SystemBus()
	if err != nil {
		return ret
	}

	obj := c.Object("org.freedesktop.systemd1", "/org/freedesktop/systemd1")
	call := obj.Call("org.freedesktop.systemd1.Manager.GetUnit", 0, m.name)
	if call != nil && call.Err != nil {
		log.Println(err)
		return ret
	}

	if err := call.Store(&ret); err != nil {
		log.Println(err)
		return ret
	}

	return ret
}

func (m *unitMonitor) getProp(name string) dbus.Variant {

	var ret dbus.Variant

	c, err := dbus.SystemBus()
	if err != nil {
		log.Println(err)
		return ret
	}

	err = c.Object("org.freedesktop.systemd1", m.path).Call(
		propGet,
		0,
		"org.freedesktop.systemd1.Unit",
		name,
	).Store(&ret)

	if err != nil {
		log.Println(err)
	}
	return ret
}

func (m *unitMonitor) generateStatus() serviceStatus {
	status := serviceStatus{
		ID:   getLocalIP(),
		Name: m.name,
		Path: m.path,
	}

	var prop dbus.Variant
	prop = m.getProp("ActiveState")
	if prop.Value() != nil {
		status.ActiveState = prop.Value().(string)
	}

	prop = m.getProp("ActiveEnterTimestamp")
	if prop.Value() != nil {
		status.ActiveEnterTimestamp = prop.Value().(uint64)
	}

	prop = m.getProp("ActiveExitTimestamp")
	if prop.Value() != nil {
		status.ActiveExitTimestamp = prop.Value().(uint64)
	}

	prop = m.getProp("InactiveEnterTimestamp")
	if prop.Value() != nil {
		status.InactiveEnterTimestamp = prop.Value().(uint64)
	}

	prop = m.getProp("InactiveExitTimestamp")
	if prop.Value() != nil {
		status.InactiveExitTimestamp = prop.Value().(uint64)
	}

	return status
}

func (m *unitMonitor) watch() error {

	conn, err := dbus.SystemBus()
	if err != nil {
		return err
	}

	m.path = m.getUnitPath()

	// subscribe the props changes signal
	props := fmt.Sprintf("type='signal',path='%s',interface='org.freedesktop.DBus.Properties'", m.path)
	call := conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, props)
	if call != nil && call.Err != nil {
		return call.Err
	}

	// unsubscribe on return
	defer func() {
		conn.BusObject().Call("org.freedesktop.DBus.RemoveMatch", 0, props)
	}()

	signal := make(chan *dbus.Signal, 10)
	defer close(signal)

	conn.Signal(signal)

	log.Printf("watching for %s @%s\n", m.name, m.path)

	for {
		select {
		case ev := <-signal:
			switch ev.Name {
			case propertiesChanged:
				var iName string
				var changedProps map[string]dbus.Variant
				var invProps []string

				if ev.Path == m.path {
					if err := dbus.Store(ev.Body, &iName, &changedProps, &invProps); err != nil {
						log.Println(err.Error())
						log.Println("aku dead")
						continue
					}

					if iName == "org.freedesktop.systemd1.Unit" {
						m.chanPub <- m.generateStatus()
					}
				}
			}

		case <-m.done:
			return nil

		}
	}

	return nil
}

func waitForOsSignal() {
	signals := []os.Signal{syscall.SIGINT, syscall.SIGKILL, syscall.SIGTERM, syscall.SIGSTOP}
	signalSink := make(chan os.Signal, 1)
	defer close(signalSink)

	signal.Notify(signalSink, signals...)
	<-signalSink
}

func getLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, address := range addrs {
		// check the address type and if it is not a loopback the display it
		if ipnet, ok := address.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}
	return ""
}

func watchServices(chanDone chan struct{}, units ...string) {
	chanPub := make(chan serviceStatus, 100)
	defer close(chanPub)

	for _, unit := range units {
		go func(u string) {
			m := &unitMonitor{
				done:    chanDone,
				name:    u,
				chanPub: chanPub,
			}

			if err := m.watch(); err != nil {
				log.Println(u, err)
			}

		}(unit)
	}

	for {
		select {
		case status := <-chanPub:
			// TODO: this is my personal implementation only. Please modify to suit your needs
			contents, _ := ioutil.ReadFile("filename.txt")
			println(string(contents))
			ioutil.WriteFile("filename.txt", []byte(status.ActiveState), 0644)
			if status.ActiveState != string(contents) {
				slackLogger.Info(status.String())
				log.Println("aku hore")
			}
		case <-chanDone:
			return
		}
	}
}

var (
	slackLogger slackconnect.Logger
)

func main() {
	webhookUri := os.Getenv(envWebhookUri)
	if webhookUri == "" {
		fmt.Fprintf(os.Stderr, "Please set %s to your valid slack webhook uri\n", envWebhookUri)
		os.Exit(-1)
	}

	slackLogger = slackconnect.NewLogger(webhookUri, "systemd.db", "#only_for_testing", "MSA-BOT", nil)
	done := make(chan struct{})
	defer close(done)
	defer slackLogger.Close()

	// sample services
	units := []string{"httpd.service"}

	ose := os.Getenv(envServices)
	if ose != "" {
		units = []string{}
		for _, s := range strings.Split(ose, ",") {
			units = append(units, strings.TrimSpace(s))
		}
	}

	if len(units) == 0 {
		os.Exit(-1)
	}

	slackLogger.Open()

	go watchServices(done, units...)
	waitForOsSignal()
}
