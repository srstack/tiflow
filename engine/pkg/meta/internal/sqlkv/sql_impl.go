// Copyright 2022 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package sqlkv

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	sqlkvModel "github.com/pingcap/tiflow/engine/pkg/meta/internal/sqlkv/model"
	metaModel "github.com/pingcap/tiflow/engine/pkg/meta/model"
	"github.com/pingcap/tiflow/engine/pkg/orm"
	ormModel "github.com/pingcap/tiflow/engine/pkg/orm/model"
	cerrors "github.com/pingcap/tiflow/pkg/errors"
)

// sqlKVClientImpl is the mysql-compatible implement for KVClient
type sqlKVClientImpl struct {
	// db is the original gorm.DB without table scope
	db    *gorm.DB
	jobID metaModel.JobID
	// tableScopeDB is with project-specific metakv table scope
	// we use it in all methods except GenEpoch
	// since GenEpoch use a different backend table
	tableScopeDB *gorm.DB

	// for GenEpoch
	epochClient ormModel.EpochClient
}

// NewSQLKVClientImpl new a sql implement for kvclient
func NewSQLKVClientImpl(sqlDB *sql.DB, table string, jobID metaModel.JobID) (*sqlKVClientImpl, error) {
	if sqlDB == nil {
		return nil, cerrors.ErrMetaParamsInvalid.GenWithStackByArgs("input db is nil")
	}

	db, err := orm.NewGormDB(sqlDB)
	if err != nil {
		return nil, err
	}

	tableScopeDB := db
	if table != "" {
		tableScopeDB = db.Table(table)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	impl := &sqlKVClientImpl{
		db:           db,
		jobID:        jobID,
		tableScopeDB: tableScopeDB,
	}
	if err := impl.Initialize(ctx); err != nil {
		return nil, err
	}

	return impl, nil
}

// Initialize initializes metakv table
// NOTE: Make Sure to call InitializeEpochModel before Initialize any KVClient
func (c *sqlKVClientImpl) Initialize(ctx context.Context) error {
	if err := c.tableScopeDB.
		WithContext(ctx).
		AutoMigrate(&sqlkvModel.MetaKV{}); err != nil {
		return cerrors.ErrMetaOpFail.Wrap(err)
	}

	epCli, err := ormModel.NewEpochClient(c.jobID, c.db)
	if err != nil {
		return err
	}
	c.epochClient = epCli

	return nil
}

// Close implements Close interface of Client
func (c *sqlKVClientImpl) Close() error {
	return nil
}

// GetEpoch implements GenEpoch interface of Client
func (c *sqlKVClientImpl) GenEpoch(ctx context.Context) (int64, error) {
	return c.epochClient.GenEpoch(ctx)
}

// Put implements Put interface of KV
func (c *sqlKVClientImpl) Put(ctx context.Context, key, val string) (*metaModel.PutResponse, metaModel.Error) {
	op := metaModel.OpPut(key, val)
	return c.doPut(ctx, c.tableScopeDB, &op)
}

func (c *sqlKVClientImpl) doPut(ctx context.Context, db *gorm.DB, op *metaModel.Op) (*metaModel.PutResponse, metaModel.Error) {
	if err := db.WithContext(ctx).
		Clauses(clause.OnConflict{
			UpdateAll: true,
		}).Create(&sqlkvModel.MetaKV{
		JobID: c.jobID,
		KeyValue: metaModel.KeyValue{
			Key:   op.KeyBytes(),
			Value: op.ValueBytes(),
		},
	}).Error; err != nil {
		return nil, sqlErrorFromOpFail(err)
	}

	return &metaModel.PutResponse{
		Header: &metaModel.ResponseHeader{},
	}, nil
}

// Get implements Get interface of KV
func (c *sqlKVClientImpl) Get(ctx context.Context, key string, opts ...metaModel.OpOption) (*metaModel.GetResponse, metaModel.Error) {
	op := metaModel.OpGet(key, opts...)
	return c.doGet(ctx, c.tableScopeDB, &op)
}

func (c *sqlKVClientImpl) doGet(ctx context.Context, db *gorm.DB, op *metaModel.Op) (*metaModel.GetResponse, metaModel.Error) {
	if err := op.CheckValidOp(); err != nil {
		return nil, &sqlError{
			displayed: cerrors.ErrMetaOptionInvalid.Wrap(err),
		}
	}

	var (
		metaKvs    []*sqlkvModel.MetaKV
		metaKv     sqlkvModel.MetaKV
		err        error
		isPointGet bool
		key        = op.KeyBytes()
	)

	db = db.WithContext(ctx).Where("job_id = ?", c.jobID)
	switch {
	case op.IsOptsWithRange():
		err = db.Where("key >= ? AND key < ?", key, op.RangeBytes()).Find(&metaKvs).Error
	case op.IsOptsWithPrefix():
		err = db.Where("key like ?%", key).Find(&metaKvs).Error
	case op.IsOptsWithFromKey():
		err = db.Where("key >= ?", key).Find(&metaKvs).Error
	default:
		err = db.Where("key = ?", key).First(&metaKv).Error
		isPointGet = true
	}
	if err != nil {
		return nil, sqlErrorFromOpFail(err)
	}

	var kvs []*metaModel.KeyValue
	if isPointGet {
		kvs = make([]*metaModel.KeyValue, 0, 1)
		kvs = append(kvs, &metaModel.KeyValue{Key: metaKv.KeyValue.Key, Value: metaKv.KeyValue.Value})
	} else {
		kvs = make([]*metaModel.KeyValue, 0, len(metaKvs))
		for _, metaKv := range metaKvs {
			kvs = append(kvs, &metaModel.KeyValue{Key: metaKv.KeyValue.Key, Value: metaKv.KeyValue.Value})
		}
	}

	return &metaModel.GetResponse{
		Header: &metaModel.ResponseHeader{},
		Kvs:    kvs,
	}, nil
}

// Delete implements Delete interface of KV
func (c *sqlKVClientImpl) Delete(ctx context.Context, key string, opts ...metaModel.OpOption) (*metaModel.DeleteResponse, metaModel.Error) {
	op := metaModel.OpDelete(key, opts...)
	return c.doDelete(ctx, c.tableScopeDB, &op)
}

func (c *sqlKVClientImpl) doDelete(ctx context.Context, db *gorm.DB, op *metaModel.Op) (*metaModel.DeleteResponse, metaModel.Error) {
	if err := op.CheckValidOp(); err != nil {
		return nil, &sqlError{
			displayed: cerrors.ErrMetaOptionInvalid.Wrap(err),
		}
	}

	var (
		err error
		key = op.KeyBytes()
	)

	db = db.WithContext(ctx).Where("job_id = ?", c.jobID)
	switch {
	case op.IsOptsWithRange():
		err = db.Where("key >= ? AND key < ?", key,
			op.RangeBytes()).Delete(&sqlkvModel.MetaKV{}).Error
	case op.IsOptsWithPrefix():
		err = db.Where("key like ?%", key).Delete(&sqlkvModel.MetaKV{}).Error
	case op.IsOptsWithFromKey():
		err = db.Where("key >= ?", key).Delete(&sqlkvModel.MetaKV{}).Error
	default:
		err = db.Where("key = ?", key).Delete(&sqlkvModel.MetaKV{}).Error
	}
	if err != nil {
		return nil, sqlErrorFromOpFail(err)
	}

	return &metaModel.DeleteResponse{
		Header: &metaModel.ResponseHeader{},
	}, nil
}

type sqlTxn struct {
	mu sync.Mutex

	ctx  context.Context
	impl *sqlKVClientImpl
	ops  []metaModel.Op
	// cache error to make chain operation work
	Err       *sqlError
	committed bool
}

// Txn implements Txn interface of KV
func (c *sqlKVClientImpl) Txn(ctx context.Context) metaModel.Txn {
	return &sqlTxn{
		ctx:  ctx,
		impl: c,
		ops:  make([]metaModel.Op, 0, 2),
	}
}

// Do implements Do interface of Txn
func (t *sqlTxn) Do(ops ...metaModel.Op) metaModel.Txn {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.Err != nil {
		return t
	}

	if t.committed {
		t.Err = &sqlError{
			displayed: cerrors.ErrMetaCommittedTxn.GenWithStackByArgs("txn had been committed"),
		}
		return t
	}

	t.ops = append(t.ops, ops...)
	return t
}

// Commit implements Commit interface of Txn
func (t *sqlTxn) Commit() (*metaModel.TxnResponse, metaModel.Error) {
	t.mu.Lock()
	if t.Err != nil {
		t.mu.Unlock()
		return nil, t.Err
	}
	if t.committed {
		t.Err = &sqlError{
			displayed: cerrors.ErrMetaCommittedTxn.GenWithStackByArgs("txn had been committed"),
		}
		t.mu.Unlock()
		return nil, t.Err
	}
	t.committed = true
	t.mu.Unlock()

	var txnRsp metaModel.TxnResponse
	txnRsp.Responses = make([]metaModel.ResponseOp, 0, len(t.ops))
	err := t.impl.tableScopeDB.Transaction(func(tx *gorm.DB) error {
		for _, op := range t.ops {
			switch {
			case op.IsGet():
				rsp, err := t.impl.doGet(t.ctx, tx, &op)
				if err != nil {
					return err // rollback
				}
				txnRsp.Responses = append(txnRsp.Responses, makeGetResponseOp(rsp))
			case op.IsPut():
				rsp, err := t.impl.doPut(t.ctx, tx, &op)
				if err != nil {
					return err
				}
				txnRsp.Responses = append(txnRsp.Responses, makePutResponseOp(rsp))
			case op.IsDelete():
				rsp, err := t.impl.doDelete(t.ctx, tx, &op)
				if err != nil {
					return err
				}
				txnRsp.Responses = append(txnRsp.Responses, makeDelResponseOp(rsp))
			case op.IsTxn():
				return &sqlError{
					displayed: cerrors.ErrMetaNestedTxn.GenWithStackByArgs("unsupported nested txn"),
				}
			default:
				return &sqlError{
					displayed: cerrors.ErrMetaOpFail.GenWithStackByArgs("unknown op type"),
				}
			}
		}

		return nil // commit
	})
	if err != nil {
		err2, ok := err.(*sqlError)
		if ok {
			return nil, err2
		}

		return nil, sqlErrorFromOpFail(err2)
	}

	return &txnRsp, nil
}
