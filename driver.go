package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"github.com/containerd/fifo"
	"github.com/docker/docker/api/types/plugins/logdriver"
	"github.com/docker/docker/daemon/logger"
	"github.com/docker/docker/daemon/logger/jsonfilelog"
	protoio "github.com/gogo/protobuf/io"
	"github.com/pkg/errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type driver struct {
	sLog *slog.Logger

	mu                         sync.Mutex
	fileToLogWrapperMap        map[string]*logPair
	containerIdToLogWrapperMap map[string]*logPair
}

type logPair struct {
	jsonl   logger.Logger
	gLogger logger.Logger
	logFile io.ReadCloser
	info    logger.Info
}

func createDriver() *driver {
	return &driver{
		fileToLogWrapperMap:        make(map[string]*logPair),
		containerIdToLogWrapperMap: make(map[string]*logPair),
		sLog:                       slog.Default(),
	}
}

func (d *driver) logAndReturnError(err error, msg string) error {
	d.sLog.With("error", err).Error(msg)
	return err
}

func (d *driver) StartLogging(file string, info logger.Info) error {
	logFileReader, err := fifo.OpenFifo(context.Background(), file, syscall.O_RDONLY, 0700)
	if err != nil {
		return d.logAndReturnError(err, "Error opening log file")
	}

	if info.LogPath == "" {
		info.LogPath = filepath.Join("/var/log/docker", info.ContainerID)
	}
	if err = os.MkdirAll(filepath.Dir(info.LogPath), 0755); err != nil {
		return errors.Wrap(err, "error setting up logger dir")
	}

	jsonl, err := jsonfilelog.New(info)
	if err != nil {
		return d.logAndReturnError(err, "Error creating JSON file for local logging")
	}

	gLogger, err := New(info)
	if err != nil {
		return d.logAndReturnError(err, "Error creating GCP logger")
	}

	d.mu.Lock()
	lf := &logPair{
		jsonl:   jsonl,
		gLogger: gLogger,
		logFile: logFileReader,
		info:    info,
	}
	d.fileToLogWrapperMap[file] = lf
	d.containerIdToLogWrapperMap[info.ContainerID] = lf
	d.mu.Unlock()

	go d.consumeLog(lf)
	return nil
}

func (d *driver) consumeLog(lp *logPair) {
	dec := protoio.NewUint32DelimitedReader(lp.logFile, binary.BigEndian, 1e6)
	defer dec.Close()
	var buf logdriver.LogEntry
	for {
		if err := dec.ReadMsg(&buf); err != nil {
			if err == io.EOF {
				d.sLog.With("id", lp.info.ContainerID).With("error", err).
					Debug("shutting down log gLogger")
				lp.logFile.Close()
				return
			}
			dec = protoio.NewUint32DelimitedReader(lp.logFile, binary.BigEndian, 1e6)
			continue
		}

		var msg logger.Message
		msg.Line = buf.Line
		msg.Source = buf.Source
		if buf.PartialLogMetadata != nil {
			msg.PLogMetaData.ID = buf.PartialLogMetadata.Id
			msg.PLogMetaData.Last = buf.PartialLogMetadata.Last
			msg.PLogMetaData.Ordinal = int(buf.PartialLogMetadata.Ordinal)
		}
		msg.Timestamp = time.Unix(0, buf.TimeNano)

		if err := lp.gLogger.Log(&msg); err != nil {
			d.sLog.With("id", lp.info.ContainerID, "error", err, "message", msg).Error("error writing log to GCP logger message")
			continue
		}
		if localLoggingEnabled {
			if err := lp.jsonl.Log(&msg); err != nil {
				d.sLog.With("id", lp.info.ContainerID, "error", err, "message", msg).Error("error writing log message to JSON logger")
				continue
			}
		}

		buf.Reset()
	}
}

func (d *driver) StopLogging(file string) (err error) {
	d.sLog.With("file", file).Debug("Stopping logging")
	d.mu.Lock()
	lf, ok := d.fileToLogWrapperMap[file]
	if ok {
		err = lf.gLogger.(*nGCPLogger).logger.Flush()
		if err != nil {
			d.sLog.With("error", err).Error("Error flushing GCP logger during shutdown")
		}
		err = lf.logFile.Close()
		if err != nil {
			d.sLog.With("error", err).Error("Error closing log file")
		}
		delete(d.fileToLogWrapperMap, file)
	}
	d.mu.Unlock()
	return err
}

func (d *driver) ReadLogs(info logger.Info, config logger.ReadConfig) (io.ReadCloser, error) {
	d.mu.Lock()
	lf, exists := d.containerIdToLogWrapperMap[info.ContainerID]
	d.mu.Unlock()
	if !exists {
		return nil, fmt.Errorf("logger does not exist for %s", info.ContainerID)
	}

	r, w := io.Pipe()
	lr, ok := lf.jsonl.(logger.LogReader)
	if !ok {
		return nil, fmt.Errorf("logger does not support reading")
	}

	go func() {
		watcher := lr.ReadLogs(config)

		enc := protoio.NewUint32DelimitedWriter(w, binary.BigEndian)
		defer enc.Close()
		defer watcher.ConsumerGone()

		var buf logdriver.LogEntry
		for {
			select {
			case msg, ok := <-watcher.Msg:
				if !ok {
					w.Close()
					return
				}

				buf.Line = msg.Line
				buf.Partial = msg.PLogMetaData != nil
				buf.TimeNano = msg.Timestamp.UnixNano()
				buf.Source = msg.Source

				if err := enc.WriteMsg(&buf); err != nil {
					w.CloseWithError(err)
					return
				}
			case err := <-watcher.Err:
				w.CloseWithError(err)
				return
			}

			buf.Reset()
		}
	}()

	return r, nil
}
