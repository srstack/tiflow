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

package servermaster

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/pingcap/log"
	"go.uber.org/zap"

	metaModel "github.com/pingcap/tiflow/engine/pkg/meta/model"
	"github.com/pingcap/tiflow/pkg/errors"
	"github.com/pingcap/tiflow/pkg/logutil"
	"github.com/pingcap/tiflow/pkg/security"
)

const (
	defaultSessionTTL        = 5 * time.Second
	defaultKeepAliveTTL      = "20s"
	defaultKeepAliveInterval = "500ms"
	defaultRPCTimeout        = "3s"
	defaultCampaignTimeout   = 5 * time.Second
	defaultDiscoverTicker    = 3 * time.Second
	defaultMetricInterval    = 15 * time.Second
	defaultMasterAddr        = "127.0.0.1:10240"

	// DefaultBusinessMetaID is the ID for default business metastore
	DefaultBusinessMetaID        = "_default"
	defaultBusinessMetaEndpoints = "127.0.0.1:12479"

	// FrameMetaID is the ID for frame metastore
	FrameMetaID               = "_root"
	defaultFrameMetaEndpoints = "127.0.0.1:3336"
	defaultFrameMetaUser      = "root"
	defaultFrameMetaPassword  = "123456"

	defaultFrameworkStoreType = metaModel.StoreTypeSQL
	// TODO: we will switch to StoreTypeSQL after we support sql implement
	defaultbusinessStoreType = metaModel.StoreTypeEtcd
)

// Config is the configuration for server-master.
type Config struct {
	LogConf logutil.Config `toml:"log" json:"log"`

	Addr          string `toml:"addr" json:"addr"`
	AdvertiseAddr string `toml:"advertise-addr" json:"advertise-addr"`

	ETCDEndpoints []string `toml:"etcd-endpoints" json:"etcd-endpoints"`

	FrameMetaConf    *metaModel.StoreConfig `toml:"frame-metastore-conf" json:"frame-metastore-conf"`
	BusinessMetaConf *metaModel.StoreConfig `toml:"business-metastore-conf" json:"business-metastore-conf"`

	KeepAliveTTLStr string `toml:"keepalive-ttl" json:"keepalive-ttl"`
	// time interval string to check executor aliveness
	KeepAliveIntervalStr string `toml:"keepalive-interval" json:"keepalive-interval"`
	RPCTimeoutStr        string `toml:"rpc-timeout" json:"rpc-timeout"`

	KeepAliveTTL      time.Duration `toml:"-" json:"-"`
	KeepAliveInterval time.Duration `toml:"-" json:"-"`
	RPCTimeout        time.Duration `toml:"-" json:"-"`

	Security *security.Credential `toml:"security" json:"security"`
}

func (c *Config) String() string {
	cfg, err := json.Marshal(c)
	if err != nil {
		log.Error("marshal to json", zap.Reflect("master config", c), logutil.ShortError(err))
	}
	return string(cfg)
}

// Toml returns TOML format representation of config.
func (c *Config) Toml() (string, error) {
	var b bytes.Buffer

	err := toml.NewEncoder(&b).Encode(c)
	if err != nil {
		log.Error("fail to marshal config to toml", logutil.ShortError(err))
	}

	return b.String(), nil
}

// Adjust adjusts the master configuration
func (c *Config) Adjust() (err error) {
	if c.AdvertiseAddr == "" {
		c.AdvertiseAddr = c.Addr
	}

	c.KeepAliveInterval, err = time.ParseDuration(c.KeepAliveIntervalStr)
	if err != nil {
		return err
	}

	c.KeepAliveTTL, err = time.ParseDuration(c.KeepAliveTTLStr)
	if err != nil {
		return err
	}

	c.RPCTimeout, err = time.ParseDuration(c.RPCTimeoutStr)
	if err != nil {
		return err
	}

	c.adjustStoreConfig()
	return nil
}

// adjustStoreConfig adjusts store configuration
func (c *Config) adjustStoreConfig() {
	strings.ToLower(strings.TrimSpace(c.FrameMetaConf.StoreType))
	strings.ToLower(strings.TrimSpace(c.BusinessMetaConf.StoreType))
}

// configFromFile loads config from file and merges items into Config.
func (c *Config) configFromFile(path string) error {
	metaData, err := toml.DecodeFile(path, c)
	if err != nil {
		return errors.WrapError(errors.ErrMasterDecodeConfigFile, err)
	}
	return checkUndecodedItems(metaData)
}

func (c *Config) configFromString(data string) error {
	metaData, err := toml.Decode(data, c)
	if err != nil {
		return errors.WrapError(errors.ErrMasterDecodeConfigFile, err)
	}
	return checkUndecodedItems(metaData)
}

// GetDefaultMasterConfig returns a default master config
func GetDefaultMasterConfig() *Config {
	return &Config{
		LogConf: logutil.Config{
			Level: "info",
			File:  "",
		},
		Addr:                 defaultMasterAddr,
		AdvertiseAddr:        "",
		FrameMetaConf:        newFrameMetaConfig(),
		BusinessMetaConf:     NewDefaultBusinessMetaConfig(),
		KeepAliveTTLStr:      defaultKeepAliveTTL,
		KeepAliveIntervalStr: defaultKeepAliveInterval,
		RPCTimeoutStr:        defaultRPCTimeout,
	}
}

func checkUndecodedItems(metaData toml.MetaData) error {
	undecoded := metaData.Undecoded()
	if len(undecoded) > 0 {
		var undecodedItems []string
		for _, item := range undecoded {
			undecodedItems = append(undecodedItems, item.String())
		}
		return errors.ErrMasterConfigUnknownItem.GenWithStackByArgs(strings.Join(undecodedItems, ","))
	}
	return nil
}

// newFrameMetaConfig return the default framework metastore config
func newFrameMetaConfig() *metaModel.StoreConfig {
	conf := metaModel.DefaultStoreConfig()
	conf.StoreID = FrameMetaID
	conf.StoreType = defaultFrameworkStoreType
	conf.Endpoints = append(conf.Endpoints, defaultFrameMetaEndpoints)
	conf.Auth.User = defaultFrameMetaUser
	conf.Auth.Passwd = defaultFrameMetaPassword

	return conf
}

// NewDefaultBusinessMetaConfig return the default business metastore config
func NewDefaultBusinessMetaConfig() *metaModel.StoreConfig {
	conf := metaModel.DefaultStoreConfig()
	conf.StoreID = DefaultBusinessMetaID
	conf.StoreType = defaultbusinessStoreType
	conf.Endpoints = append(conf.Endpoints, defaultBusinessMetaEndpoints)

	return conf
}
