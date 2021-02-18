package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/opencontainers/runc/libcontainer"
	"github.com/urfave/cli"

	"github.com/sirupsen/logrus"
	"time"
	//"github.com/hodgesds/perf-utils"
)

var startCommand = cli.Command{
	Name:  "start",
	Usage: "executes the user defined process in a created container",
	ArgsUsage: `<container-id>

Where "<container-id>" is your name for the instance of the container that you
are starting. The name you provide for the container instance must be unique on
your host.`,
	Description: `The start command executes the user defined process in a created container.`,
	Action: func(context *cli.Context) error {
		logrus.Info(fmt.Sprintf("start.go  %v",int64(time.Nanosecond) * time.Now().UnixNano() / int64(time.Millisecond)))
		if err := checkArgs(context, 1, exactArgs); err != nil {
			return err
		}
		logrus.Info(fmt.Sprintf("getContainer starts %v",int64(time.Nanosecond) * time.Now().UnixNano() / int64(time.Millisecond)))
		container, err := getContainer(context)
		/*
		var container libcontainer.Container;
		var err error;
		profileValue, prof_err := perf.CPUInstructions(
			func() error{
				container,err = getContainer(context)
				return nil
			},
		)
		logrus.Info(fmt.Sprintf("CPU instructions %+v",profileValue))
		logrus.Info(fmt.Sprintf("getContainer ends %v",int64(time.Nanosecond) * time.Now().UnixNano() / int64(time.Millisecond)))
		if prof_err != nil{
			logrus.Info(prof_err)
		}
		*/
		//test
		/*
		var total int = 0
		profileValue, prof_err := perf.CPUInstructions(
			func() error{
				total = 2
				return nil
			},
		)
		logrus.Info(fmt.Sprintf("CPU instructions %+v, total %d",profileValue,total))
		if prof_err != nil{
			logrus.Info(prof_err)
		}*/
		//
		if err != nil {
			return err
		}
		status, err := container.Status()
		if err != nil {
			return err
		}
		switch status {
		case libcontainer.Created:
			notifySocket, err := notifySocketStart(context, os.Getenv("NOTIFY_SOCKET"), container.ID())
			if err != nil {
				return err
			}
			logrus.Info(fmt.Sprintf("container.Exec() starts %v",int64(time.Nanosecond) * time.Now().UnixNano() / int64(time.Millisecond)))
			if err := container.Exec(); err != nil {
				return err
			}
			logrus.Info(fmt.Sprintf("container.Exec() ends %v",int64(time.Nanosecond) * time.Now().UnixNano() / int64(time.Millisecond)))
			if notifySocket != nil {
				return notifySocket.waitForContainer(container)
			}
			return nil
		case libcontainer.Stopped:
			return errors.New("cannot start a container that has stopped")
		case libcontainer.Running:
			return errors.New("cannot start an already running container")
		default:
			return fmt.Errorf("cannot start a container in the %s state\n", status)
		}
	},
}
