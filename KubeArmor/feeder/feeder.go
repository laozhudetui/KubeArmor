package feeder

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	kl "github.com/accuknox/KubeArmor/KubeArmor/common"
	kg "github.com/accuknox/KubeArmor/KubeArmor/log"
	tp "github.com/accuknox/KubeArmor/KubeArmor/types"

	pb "github.com/accuknox/KubeArmor/protobuf"
	"github.com/google/uuid"
	"google.golang.org/grpc"
)

// ============ //
// == Global == //
// ============ //

// Running flag
var Running bool

// MsgQueue for Messages
var MsgQueue []pb.Message

// MsgLock for Messages
var MsgLock sync.Mutex

// LogQueue for Logs
var LogQueue []pb.Log

// LogLock for Logs
var LogLock sync.Mutex

func init() {
	Running = true

	MsgQueue = []pb.Message{}
	MsgLock = sync.Mutex{}

	LogQueue = []pb.Log{}
	LogLock = sync.Mutex{}
}

// ========== //
// == gRPC == //
// ========== //

// MsgStruct Structure
type MsgStruct struct {
	Client pb.LogService_WatchMessagesServer
	Filter string
}

// LogStruct Structure
type LogStruct struct {
	Client pb.LogService_WatchLogsServer
	Filter string
}

// LogService Structure
type LogService struct {
	MsgStructs map[string]MsgStruct
	MsgLock    sync.Mutex

	LogStructs map[string]LogStruct
	LogLock    sync.Mutex
}

// HealthCheck Function
func (ls *LogService) HealthCheck(ctx context.Context, nonce *pb.NonceMessage) (*pb.ReplyMessage, error) {
	replyMessage := pb.ReplyMessage{Retval: nonce.Nonce}
	return &replyMessage, nil
}

// addMsgStruct Function
func (ls *LogService) addMsgStruct(uid string, srv pb.LogService_WatchMessagesServer, filter string) {
	ls.MsgLock.Lock()
	defer ls.MsgLock.Unlock()

	msgStruct := MsgStruct{}
	msgStruct.Client = srv
	msgStruct.Filter = filter

	ls.MsgStructs[uid] = msgStruct
}

// removeMsgStruct Function
func (ls *LogService) removeMsgStruct(uid string) {
	ls.MsgLock.Lock()
	defer ls.MsgLock.Unlock()

	delete(ls.MsgStructs, uid)
}

// getMsgStructs Function
func (ls *LogService) getMsgStructs() []MsgStruct {
	msgStructs := []MsgStruct{}

	ls.MsgLock.Lock()
	defer ls.MsgLock.Unlock()

	for _, mgs := range ls.MsgStructs {
		msgStructs = append(msgStructs, mgs)
	}

	return msgStructs
}

// WatchMessages Function
func (ls *LogService) WatchMessages(req *pb.RequestMessage, svr pb.LogService_WatchMessagesServer) error {
	uid := uuid.Must(uuid.NewRandom()).String()

	ls.addMsgStruct(uid, svr, req.Filter)
	defer ls.removeMsgStruct(uid)

	for Running {
		MsgLock.Lock()

		msgStructs := ls.getMsgStructs()

		for len(MsgQueue) != 0 {
			msg := MsgQueue[0]
			MsgQueue = MsgQueue[1:]

			for _, mgs := range msgStructs {
				mgs.Client.Send(&msg)
			}
		}

		MsgLock.Unlock()

		time.Sleep(time.Millisecond * 1)
	}

	return nil
}

// addLogStruct Function
func (ls *LogService) addLogStruct(uid string, srv pb.LogService_WatchLogsServer, filter string) {
	ls.LogLock.Lock()
	defer ls.LogLock.Unlock()

	logStruct := LogStruct{}
	logStruct.Client = srv
	logStruct.Filter = filter

	ls.LogStructs[uid] = logStruct
}

// removeLogStruct Function
func (ls *LogService) removeLogStruct(uid string) {
	ls.LogLock.Lock()
	defer ls.LogLock.Unlock()

	delete(ls.LogStructs, uid)
}

// getLogStructs Function
func (ls *LogService) getLogStructs() []LogStruct {
	logStructs := []LogStruct{}

	ls.LogLock.Lock()
	defer ls.LogLock.Unlock()

	for _, lgs := range ls.LogStructs {
		logStructs = append(logStructs, lgs)
	}

	return logStructs
}

// WatchLogs Function
func (ls *LogService) WatchLogs(req *pb.RequestMessage, svr pb.LogService_WatchLogsServer) error {
	uid := uuid.Must(uuid.NewRandom()).String()

	ls.addLogStruct(uid, svr, req.Filter)
	defer ls.removeLogStruct(uid)

	for Running {
		LogLock.Lock()

		logStructs := ls.getLogStructs()

		for len(LogQueue) != 0 {
			log := LogQueue[0]
			LogQueue = LogQueue[1:]

			for _, lgs := range logStructs {
				if lgs.Filter == "" {
					lgs.Client.Send(&log)
				} else if lgs.Filter == "policy" && (log.Type == "MatchedPolicy" || log.Type == "MatchedHostPolicy") {
					lgs.Client.Send(&log)
				} else if lgs.Filter == "system" && (log.Type == "ContainerLog" || log.Type == "HostLog") {
					lgs.Client.Send(&log)
				}
			}
		}

		LogLock.Unlock()

		time.Sleep(time.Millisecond * 1)
	}

	return nil
}

// ============ //
// == Feeder == //
// ============ //

// Feeder Structure
type Feeder struct {
	// port
	port string

	// output
	output string

	// gRPC listener
	listener net.Listener

	// log server
	logServer *grpc.Server

	// wait group
	WgServer sync.WaitGroup

	// cluster
	clusterName string

	// host
	hostName string
	hostIP   string

	// namespace name + container group name / host name -> corresponding security policies
	SecurityPolicies     map[string]tp.MatchPolicies
	SecurityPoliciesLock *sync.RWMutex

	// options
	EnableSystemLog bool
}

// NewFeeder Function
func NewFeeder(port, output string, enableSystemLog bool) *Feeder {
	fd := &Feeder{}

	fd.port = fmt.Sprintf(":%s", port)
	fd.output = output

	// output mode
	if fd.output != "stdout" && fd.output != "none" {
		// get the directory part from the path
		dirLog := filepath.Dir(fd.output)

		// create directories
		if err := os.MkdirAll(dirLog, 0755); err != nil {
			kg.Errf("Failed to create a target directory (%s, %s)", dirLog, err.Error())
			return nil
		}

		// create target file
		targetFile, err := os.Create(fd.output)
		if err != nil {
			kg.Errf("Failed to create a target file (%s, %s)", fd.output, err.Error())
			return nil
		}
		targetFile.Close()
	}

	// listen to gRPC port
	listener, err := net.Listen("tcp", fd.port)
	if err != nil {
		kg.Errf("Failed to listen a port (%s, %s)", port, err.Error())
		return nil
	}
	fd.listener = listener

	// create a log server
	fd.logServer = grpc.NewServer()

	// register a log service
	logService := &LogService{
		MsgStructs: make(map[string]MsgStruct),
		MsgLock:    sync.Mutex{},
		LogStructs: make(map[string]LogStruct),
		LogLock:    sync.Mutex{},
	}
	pb.RegisterLogServiceServer(fd.logServer, logService)

	// set wait group
	fd.WgServer = sync.WaitGroup{}

	// set cluster info
	fd.clusterName = ""

	// set host info
	fd.hostName = kl.GetHostName()
	fd.hostIP = kl.GetExternalIPAddr()

	// initialize security policies
	fd.SecurityPolicies = map[string]tp.MatchPolicies{}
	fd.SecurityPoliciesLock = new(sync.RWMutex)

	// options
	fd.EnableSystemLog = enableSystemLog

	return fd
}

// DestroyFeeder Function
func (fd *Feeder) DestroyFeeder() error {
	// stop gRPC service
	Running = false

	// wait for a while
	time.Sleep(time.Second * 1)

	// close listener
	if fd.listener != nil {
		fd.listener.Close()
		fd.listener = nil
	}

	// wait for other routines
	fd.WgServer.Wait()

	return nil
}

// ============== //
// == Messages == //
// ============== //

// Print Function
func (fd *Feeder) Print(message string) {
	fd.PushMessage("INFO", message)
	kg.Print(message)
}

// Printf Function
func (fd *Feeder) Printf(message string, args ...interface{}) {
	str := fmt.Sprintf(message, args...)
	fd.PushMessage("INFO", str)
	kg.Print(str)
}

// Debug Function
func (fd *Feeder) Debug(message string) {
	fd.PushMessage("DEBUG", message)
	kg.Debug(message)
}

// Debugf Function
func (fd *Feeder) Debugf(message string, args ...interface{}) {
	str := fmt.Sprintf(message, args...)
	fd.PushMessage("DEBUG", str)
	kg.Debug(str)
}

// Err Function
func (fd *Feeder) Err(message string) {
	fd.PushMessage("ERROR", message)
	kg.Err(message)
}

// Errf Function
func (fd *Feeder) Errf(message string, args ...interface{}) {
	str := fmt.Sprintf(message, args...)
	fd.PushMessage("ERROR", str)
	kg.Err(str)
}

// =============== //
// == Log Feeds == //
// =============== //

// ServeLogFeeds Function
func (fd *Feeder) ServeLogFeeds() {
	fd.WgServer.Add(1)
	defer fd.WgServer.Done()

	// feed logs
	fd.logServer.Serve(fd.listener)
}

// PushMessage Function
func (fd *Feeder) PushMessage(level, message string) error {
	pbMsg := pb.Message{}

	pbMsg.UpdatedTime = kl.GetDateTimeNow()

	pbMsg.ClusterName = fd.clusterName

	pbMsg.HostName = fd.hostName
	pbMsg.HostIP = fd.hostIP

	pbMsg.Level = level
	pbMsg.Message = message

	MsgLock.Lock()
	MsgQueue = append(MsgQueue, pbMsg)
	MsgLock.Unlock()

	return nil
}

// PushLog Function
func (fd *Feeder) PushLog(log tp.Log) error {
	log = fd.UpdateMatchedPolicy(log)

	if log.UpdatedTime == "" {
		return nil
	}

	// standard output / file output

	if fd.output == "stdout" {
		arr, _ := json.Marshal(log)
		fmt.Println(string(arr))
	} else if fd.output != "none" {
		arr, _ := json.Marshal(log)
		kl.StrToFile(string(arr), fd.output)
	}

	// gRPC output

	pbLog := pb.Log{}

	pbLog.UpdatedTime = log.UpdatedTime

	pbLog.ClusterName = fd.clusterName
	pbLog.HostName = log.HostName

	pbLog.NamespaceName = log.NamespaceName
	pbLog.PodName = log.PodName
	pbLog.ContainerID = log.ContainerID
	pbLog.ContainerName = log.ContainerName

	pbLog.HostPID = log.HostPID
	pbLog.PPID = log.PPID
	pbLog.PID = log.PID
	pbLog.UID = log.UID

	if len(log.PolicyName) > 0 {
		pbLog.PolicyName = log.PolicyName
	}

	if len(log.Severity) > 0 {
		pbLog.Severity = log.Severity
	}

	if len(log.Tags) > 0 {
		pbLog.Tags = log.Tags
	}

	if len(log.Message) > 0 {
		pbLog.Message = log.Message
	}

	pbLog.Type = log.Type
	pbLog.Source = log.Source
	pbLog.Operation = log.Operation
	pbLog.Resource = log.Resource

	if len(log.Data) > 0 {
		pbLog.Data = log.Data
	}

	if len(log.Action) > 0 {
		pbLog.Action = log.Action
	}

	pbLog.Result = log.Result

	LogLock.Lock()
	LogQueue = append(LogQueue, pbLog)
	LogLock.Unlock()

	return nil
}
