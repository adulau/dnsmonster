package output

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"

	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/compress"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/mosajjal/dnsmonster/util"
	metrics "github.com/rcrowley/go-metrics"
	log "github.com/sirupsen/logrus"
)

type ClickhouseConfig struct {
	ClickhouseAddress           string        `long:"clickhouseAddress"           env:"DNSMONSTER_CLICKHOUSEADDRESS"           default:"localhost:9000"                                          description:"Address of the clickhouse database to save the results"`
	ClickhouseUsername          string        `long:"clickhouseUsername"          env:"DNSMONSTER_CLICKHOUSEUSERNAME"          default:""                                                        description:"Username to connect to the clickhouse database"`
	ClickhousePassword          string        `long:"clickhousePassword"          env:"DNSMONSTER_CLICKHOUSEPASSWORD"          default:""                                                        description:"Password to connect to the clickhouse database"`
	ClickhouseDatabase          string        `long:"clickhouseDatabase"          env:"DNSMONSTER_CLICKHOUSEDATABASE"          default:"default"                                                 description:"Database to connect to the clickhouse database"`
	ClickhouseDelay             time.Duration `long:"clickhouseDelay"             env:"DNSMONSTER_CLICKHOUSEDELAY"             default:"1s"                                                      description:"Interval between sending results to ClickHouse"`
	ClickhouseDebug             bool          `long:"clickhouseDebug"             env:"DNSMONSTER_CLICKHOUSEDEBUG"             description:"Debug Clickhouse connection"`
	ClickhouseCompress          bool          `long:"clickhouseCompress"          env:"DNSMONSTER_CLICKHOUSECOMPRESS"          description:"Compress Clickhouse connection"`
	ClickhouseSecure            bool          `long:"clickhouseSecure"            env:"DNSMONSTER_CLICKHOUSESECURE"            description:"Use TLS for Clickhouse connection"`
	ClickhouseSaveFullQuery     bool          `long:"clickhouseSaveFullQuery"     env:"DNSMONSTER_CLICKHOUSESAVEFULLQUERY"     description:"Save full packet query and response in JSON format."`
	ClickhouseOutputType        uint          `long:"clickhouseOutputType"        env:"DNSMONSTER_CLICKHOUSEOUTPUTTYPE"        default:"0"                                                       description:"What should be written to clickhouse. options:\n;\t0: Disable Output\n;\t1: Enable Output without any filters\n;\t2: Enable Output and apply skipdomains logic\n;\t3: Enable Output and apply allowdomains logic\n;\t4: Enable Output and apply both skip and allow domains logic"    choice:"0" choice:"1" choice:"2" choice:"3" choice:"4"`
	ClickhouseBatchSize         uint          `long:"clickhouseBatchSize"         env:"DNSMONSTER_CLICKHOUSEBATCHSIZE"         default:"100000"                                                  description:"Minimun capacity of the cache array used to send data to clickhouse. Set close to the queries per second received to prevent allocations"`
	ClickhouseWorkers           uint          `long:"clickhouseWorkers"           env:"DNSMONSTER_CLICKHOUSEWORKERS"           default:"1"                                                       description:"Number of Clickhouse output Workers"`
	ClickhouseWorkerChannelSize uint          `long:"clickhouseWorkerChannelSize" env:"DNSMONSTER_CLICKHOUSEWORKERCHANNELSIZE" default:"100000"                                                  description:"Channel Size for each Clickhouse Worker"`
	outputChannel               chan util.DNSResult
	closeChannel                chan bool
}

func (chConfig ClickhouseConfig) initializeFlags() error {
	// this line will run at import time, before parsing the flags, hence showing up in --help as well as actually working
	_, err := util.GlobalParser.AddGroup("clickhouse_output", "ClickHouse Output", &chConfig)

	chConfig.outputChannel = make(chan util.DNSResult, util.GeneralFlags.ResultChannelSize)

	util.GlobalDispatchList = append(util.GlobalDispatchList, &chConfig)
	return err
}

// initialize function should not block. otherwise the dispatcher will get stuck
func (chConfig ClickhouseConfig) Initialize() error {
	if chConfig.ClickhouseOutputType > 0 && chConfig.ClickhouseOutputType < 5 {
		log.Info("Creating Clickhouse Output Channel")
		go chConfig.Output()
	} else {
		// we will catch this error in the dispatch loop and remove any output from the registry if they don't have the correct output type
		return errors.New("no output")
	}
	return nil
}

func (chConfig ClickhouseConfig) Close() {
	//todo: implement this
	<-chConfig.closeChannel
}

func (chConfig ClickhouseConfig) OutputChannel() chan util.DNSResult {
	return chConfig.outputChannel
}

func (chConfig ClickhouseConfig) connectClickhouseRetry() driver.Conn {
	tick := time.NewTicker(5 * time.Second)
	// don't retry connection if we're doing dry run
	if chConfig.ClickhouseOutputType == 0 {
		tick.Stop()
	}
	defer tick.Stop()
	for {
		c, err := chConfig.connectClickhouse()
		if err == nil {
			return c
		} else {
			log.Errorf("Error connecting to Clickhouse: %s", err)
		}

		// Error getting connection, wait the timer or check if we are exiting
		<-tick.C
		continue
	}
}

func (chConfig ClickhouseConfig) connectClickhouse() (driver.Conn, error) {
	compressOption := &clickhouse.Compression{Method: compress.NONE}
	tlsOption := &tls.Config{InsecureSkipVerify: util.GeneralFlags.SkipTLSVerification}
	if chConfig.ClickhouseCompress {
		compressOption = &clickhouse.Compression{Method: compress.LZ4}
	}
	if !chConfig.ClickhouseSecure {
		tlsOption = nil
	}

	connection, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{chConfig.ClickhouseAddress},
		Auth: clickhouse.Auth{
			Database: chConfig.ClickhouseDatabase,
			Username: chConfig.ClickhouseUsername,
			Password: chConfig.ClickhousePassword,
		},
		DialTimeout:     time.Second,
		MaxOpenConns:    16,
		MaxIdleConns:    5,
		ConnMaxLifetime: time.Hour,
		TLS:             tlsOption,
		Debug:           chConfig.ClickhouseDebug,
		Compression:     compressOption,
	})
	// connection, err := clickhouse.Open(fmt.Sprintf("tcp://%v?debug=%v&skip_verify=%v&secure=%v&compress=%v&username=%s&password=%s&database=%s", chConfig.ClickhouseAddress, chConfig.ClickhouseDebug, util.GeneralFlags.SkipTLSVerification, chConfig.ClickhouseSecure, chConfig.ClickhouseCompress, chConfig.ClickhouseUsername, chConfig.ClickhousePassword, chConfig.ClickhouseDatabase))
	if err != nil {
		log.Error(err)
		return connection, err
	}
	// batch, err := connection.PrepareBatch(context.Background(), "INSERT INTO DNS_LOG")
	return connection, err
}

// Main handler for Clickhouse output. the data from the dispatched output channel will reach this function
// Essentially, the function is responsible to hold an available connection ready by calling another goroutine,
// maintain the incoming data batch and try to INSERT them as quick as possible into the Clickhouse table
// the table structure of Clickhouse is hardcoded into the code so before outputing to Clickhouse, the user
// needs to make sure that there is proper Database connection and table are present. Refer to the project's
// clickhouse folder for the file tables.sql
func (chConfig ClickhouseConfig) Output() {
	for i := 0; i < int(chConfig.ClickhouseWorkers); i++ {
		util.GeneralFlags.GetWg().Add(1)
		go chConfig.clickhouseOutputWorker()
	}
}

func (chConfig ClickhouseConfig) clickhouseOutputWorker() {
	conn := chConfig.connectClickhouseRetry()
	clickhouseSentToOutput := metrics.GetOrRegisterCounter("clickhouseSentToOutput", metrics.DefaultRegistry)
	clickhouseSkipped := metrics.GetOrRegisterCounter("clickhouseSkipped", metrics.DefaultRegistry)
	clickhouseFailed := metrics.GetOrRegisterCounter("clickhouseFailed", metrics.DefaultRegistry)

	batch, err := conn.PrepareBatch(context.Background(), "INSERT INTO DNS_LOG")
	if err != nil {
		log.Error(err)
	}

	c := uint(0)
	// var now = time.Now()
	for {
		select {
		case data := <-chConfig.outputChannel:
			for _, dnsQuery := range data.DNS.Question {
				c++
				if util.CheckIfWeSkip(chConfig.ClickhouseOutputType, dnsQuery.Name) {
					clickhouseSkipped.Inc(1)
					continue
				}
				clickhouseSentToOutput.Inc(1)

				var fullQuery = ""
				if chConfig.ClickhouseSaveFullQuery {
					fullQuery = data.GetJson()
				}
				var SrcIP, DstIP uint64

				if data.IPVersion == 4 {
					SrcIP = uint64(binary.BigEndian.Uint32(data.SrcIP))
					DstIP = uint64(binary.BigEndian.Uint32(data.DstIP))
				} else {
					SrcIP = binary.BigEndian.Uint64(data.SrcIP[:8]) //limitation of clickhouse-go doesn't let us go more than 64 bits for ipv6 at the moment
					DstIP = binary.BigEndian.Uint64(data.DstIP[:8])
				}
				QR := uint8(0)
				if data.DNS.Response {
					QR = 1
				}
				edns, doBit := uint8(0), uint8(0)
				if edns0 := data.DNS.IsEdns0(); edns0 != nil {
					edns = 1
					if edns0.Do() {
						doBit = 1
					}
				}
				err := batch.Append(
					data.Timestamp, // packet timestamp
					time.Now(),     // index timestamp
					util.GeneralFlags.ServerName,
					data.IPVersion,
					SrcIP,
					DstIP,
					data.Protocol,
					QR,
					uint8(data.DNS.Opcode),
					uint16(dnsQuery.Qclass),
					uint16(dnsQuery.Qtype),
					edns,
					doBit,
					fullQuery,
					uint8(data.DNS.Rcode),
					dnsQuery.Name,
					data.PacketLength,
				)
				if err != nil {
					log.Error("Error while executing batch: %v. This error might be because of the changes in the table schema", err)
					clickhouseFailed.Inc(1)
				}
				//todo: test batch timeout here. does not work currently
				if c%chConfig.ClickhouseBatchSize == 0 { // || time.Since(now) > chConfig.ClickhouseDelay
					if err = batch.Send(); err != nil {
						log.Warnf("Error while executing batch: %v", err)
						clickhouseFailed.Inc(int64(c))
					}
					c = 0
					if batch, err = conn.PrepareBatch(context.Background(), "INSERT INTO DNS_LOG"); err != nil {
						log.Error(err)
					}
				}
			}
		case <-*util.GeneralFlags.GetExit():
			if err = batch.Send(); err != nil {
				log.Warnf("Error while executing batch: %v", err)
				clickhouseFailed.Inc(int64(c))
			}
			util.GeneralFlags.GetWg().Done()
			return
		}
	}
}

var _ = ClickhouseConfig{}.initializeFlags()
