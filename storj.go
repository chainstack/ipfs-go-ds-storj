// Copyright (C) 2021 Storj Labs, Inc.
// See LICENSE for copying information.

package storjds

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"strings"

	ds "github.com/ipfs/go-datastore"
	dsq "github.com/ipfs/go-datastore/query"
	"github.com/zeebo/errs"

	"storj.io/uplink"
)

type StorjDS struct {
	Config
	Project *uplink.Project
	logFile *os.File
	logger  *log.Logger
}

type Config struct {
	AccessGrant string
	Bucket      string
	LogFile     string
}

func NewStorjDatastore(conf Config) (*StorjDS, error) {
	logger := log.New(io.Discard, "", 0) // default no-op logger
	var logFile *os.File

	if len(conf.LogFile) > 0 {
		var err error
		logFile, err = os.OpenFile(conf.LogFile, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0666)
		if err != nil {
			return nil, fmt.Errorf("failed to create log file: %s", err)
		}
		logger = log.New(logFile, "", log.LstdFlags)
	}

	logger.Println("NewStorjDatastore")

	access, err := uplink.ParseAccess(conf.AccessGrant)
	if err != nil {
		return nil, fmt.Errorf("failed to parse access grant: %s", err)
	}

	project, err := uplink.OpenProject(context.Background(), access)
	if err != nil {
		return nil, fmt.Errorf("failed to open project: %s", err)
	}

	return &StorjDS{
		Config:  conf,
		Project: project,
		logFile: logFile,
		logger:  logger,
	}, nil
}

func (storj *StorjDS) Put(key ds.Key, value []byte) error {
	storj.logger.Printf("Put --- key: %s --- bytes: %d\n", key.String(), len(value))

	upload, err := storj.Project.UploadObject(context.Background(), storj.Bucket, storjKey(key), nil)
	if err != nil {
		return err
	}

	_, err = upload.Write(value)
	if err != nil {
		return err
	}

	return upload.Commit()
}

func (storj *StorjDS) Sync(prefix ds.Key) error {
	storj.logger.Printf("Sync --- prefix: %s\n", prefix.String())
	return nil
}

func (storj *StorjDS) Get(key ds.Key) ([]byte, error) {
	storj.logger.Printf("Get --- key: %s\n", key.String())

	download, err := storj.Project.DownloadObject(context.Background(), storj.Bucket, storjKey(key), nil)
	if err != nil {
		if isNotFound(err) {
			return nil, ds.ErrNotFound
		}
		return nil, err
	}

	return ioutil.ReadAll(download)
}

func (storj *StorjDS) Has(key ds.Key) (exists bool, err error) {
	storj.logger.Printf("Has --- key: %s\n", key.String())

	_, err = storj.Project.StatObject(context.Background(), storj.Bucket, storjKey(key))
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}

	return true, nil
}

func (storj *StorjDS) GetSize(key ds.Key) (size int, err error) {
	// Commented because this method is invoked very often and it is noisy.
	// storj.logger.Printf("GetSize --- key: %s\n", key.String())

	obj, err := storj.Project.StatObject(context.Background(), storj.Bucket, storjKey(key))
	if err != nil {
		if isNotFound(err) {
			return -1, ds.ErrNotFound
		}
		return -1, err
	}

	return int(obj.System.ContentLength), nil
}

func (storj *StorjDS) Delete(key ds.Key) error {
	storj.logger.Printf("Delete --- key: %s\n", key.String())

	_, err := storj.Project.DeleteObject(context.Background(), storj.Bucket, storjKey(key))
	if isNotFound(err) {
		// delete is idempotent
		err = nil
	}

	return err
}

func (storj *StorjDS) Query(q dsq.Query) (dsq.Results, error) {
	storj.logger.Printf("Query --- %s\n", q.String())

	if q.Orders != nil || q.Filters != nil {
		return nil, fmt.Errorf("storjds: filters or orders are not supported")
	}

	// Storj stores a "/foo" key as "foo" so we need to trim the leading "/"
	q.Prefix = strings.TrimPrefix(q.Prefix, "/")

	list := storj.Project.ListObjects(context.Background(), storj.Bucket, &uplink.ListObjectsOptions{
		Prefix:    q.Prefix,
		Recursive: true,
		System:    true, // TODO: enable only if q.ReturnsSizes = true
		// Cursor: TODO,
	})
	if list.Err() != nil {
		return nil, list.Err()
	}

	return dsq.ResultsFromIterator(q, dsq.Iterator{
		Close: func() error {
			return nil
		},
		Next: func() (dsq.Result, bool) {
			// TODO: skip offset, apply limit
			more := list.Next()
			if !more {
				if list.Err() != nil {
					return dsq.Result{Error: list.Err()}, false
				}
				return dsq.Result{}, false
			}
			entry := dsq.Entry{
				Key:  "/" + list.Item().Key,
				Size: int(list.Item().System.ContentLength),
			}
			if !q.KeysOnly {
				value, err := storj.Get(ds.NewKey(entry.Key))
				if err != nil {
					return dsq.Result{Error: err}, false
				}
				entry.Value = value
			}
			return dsq.Result{Entry: entry}, true
		},
	}), nil
}

func (storj *StorjDS) Batch() (ds.Batch, error) {
	storj.logger.Println("Batch")

	return &storjBatch{
		storj: storj,
		ops:   make(map[ds.Key]batchOp),
	}, nil
}

func (storj *StorjDS) Close() error {
	storj.logger.Println("Close")

	err := storj.Project.Close()

	if storj.logFile != nil {
		err = errs.Combine(err, storj.logFile.Close())
	}

	return err
}

func storjKey(ipfsKey ds.Key) string {
	return strings.TrimPrefix(ipfsKey.String(), "/")
}

func isNotFound(err error) bool {
	return errors.Is(err, uplink.ErrObjectNotFound)
}

type storjBatch struct {
	storj *StorjDS
	ops   map[ds.Key]batchOp
}

type batchOp struct {
	value  []byte
	delete bool
}

func (b *storjBatch) Put(key ds.Key, value []byte) error {
	b.storj.logger.Printf("BatchPut --- key: %s --- bytes: %d\n", key.String(), len(value))

	b.ops[key] = batchOp{
		value:  value,
		delete: false,
	}

	return nil
}

func (b *storjBatch) Delete(key ds.Key) error {
	b.storj.logger.Printf("BatchDelete --- key: %s\n", key.String())

	b.ops[key] = batchOp{
		value:  nil,
		delete: true,
	}

	return nil
}

func (b *storjBatch) Commit() error {
	b.storj.logger.Println("BatchCommit")

	for key, op := range b.ops {
		var err error
		if op.delete {
			err = b.storj.Delete(key)
		} else {
			err = b.storj.Put(key, op.value)
		}
		if err != nil {
			return err
		}
	}

	return nil
}

var _ ds.Batching = (*StorjDS)(nil)
