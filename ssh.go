package sshsyrup

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"

	netconn "github.com/mkishere/sshsyrup/net"
	os "github.com/mkishere/sshsyrup/os"
	"github.com/mkishere/sshsyrup/os/command"
	"github.com/mkishere/sshsyrup/sftp"
	"github.com/mkishere/sshsyrup/util/abuseipdb"
	"github.com/mkishere/sshsyrup/util/termlogger"
	"github.com/mkishere/sshsyrup/virtualfs"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/afero"
	"golang.org/x/crypto/ssh"
)

const (
	logTimeFormat string = "20060102"
)

// SSHSession stores SSH session info
type SSHSession struct {
	user          string
	src           net.Addr
	clientVersion string
	sshChan       <-chan ssh.NewChannel
	log           *log.Entry
	sys           *os.System
	term          string
	fs            afero.Fs
}

type envRequest struct {
	Name  string
	Value string
}

type ptyRequest struct {
	Term    string
	Width   uint32
	Height  uint32
	PWidth  uint32
	PHeight uint32
	Modes   string
}
type winChgRequest struct {
	Width  uint32
	Height uint32
}

type tunnelRequest struct {
	RemoteHost string
	RemotePort uint32
	LocalHost  string
	LocalPort  uint32
}

type Server struct {
	sshCfg *ssh.ServerConfig
	vfs    afero.Fs
}

var (
	ipConnCnt *netconn.IPConnCount = netconn.NewIPConnCount()
)

// NewSSHSession create new SSH connection based on existing socket connection
func NewSSHSession(nConn net.Conn, sshConfig *ssh.ServerConfig, vfs afero.Fs) (*SSHSession, error) {
	conn, chans, reqs, err := ssh.NewServerConn(nConn, sshConfig)
	if err != nil {
		return nil, err
	}
	clientIP, port, _ := net.SplitHostPort(conn.RemoteAddr().String())
	logger := log.WithFields(log.Fields{
		"user":      conn.User(),
		"srcIP":     clientIP,
		"port":      port,
		"clientStr": string(conn.ClientVersion()),
		"sessionId": base64.StdEncoding.EncodeToString(conn.SessionID()),
	})
	logger.Infof("New SSH connection with client")

	go ssh.DiscardRequests(reqs)
	return &SSHSession{
		user:          conn.User(),
		src:           conn.RemoteAddr(),
		clientVersion: string(conn.ClientVersion()),
		sshChan:       chans,
		log:           logger,
		fs:            vfs,
	}, nil
}

func (s *SSHSession) handleNewSession(newChan ssh.NewChannel) {

	channel, requests, err := newChan.Accept()
	if err != nil {
		s.log.WithError(err).Error("Could not accept channel")
		return
	}
	var sh *os.Shell
	go func(in <-chan *ssh.Request, channel ssh.Channel) {
		quitSignal := make(chan int, 1)
		for {
			select {
			case req := <-in:
				if req == nil {
					return
				}
				switch req.Type {
				case "winadj@putty.projects.tartarus.org", "simple@putty.projects.tartarus.org":
					//Do nothing here
				case "pty-req":
					// Of coz we are not going to create a PTY here as we are honeypot.
					// We are creating a pseudo-PTY
					var ptyreq ptyRequest
					if err := ssh.Unmarshal(req.Payload, &ptyreq); err != nil {
						s.log.WithField("reqType", req.Type).WithError(err).Errorln("Cannot parse user request payload")
						req.Reply(false, nil)
					} else {
						s.log.WithField("reqType", req.Type).Infof("User requesting pty(%v %vx%v)", ptyreq.Term, ptyreq.Width, ptyreq.Height)

						s.sys = os.NewSystem(s.user, viper.GetString("server.hostname"), s.fs, channel, int(ptyreq.Width), int(ptyreq.Height), s.log)
						s.term = ptyreq.Term
						req.Reply(true, nil)
					}
				case "env":
					var envReq envRequest
					if err := ssh.Unmarshal(req.Payload, &envReq); err != nil {
						req.Reply(false, nil)
					} else {
						s.log.WithFields(log.Fields{
							"reqType":     req.Type,
							"envVarName":  envReq.Name,
							"envVarValue": envReq.Value,
						}).Infof("User sends envvar:%v=%v", envReq.Name, envReq.Value)
						req.Reply(true, nil)
					}
				case "shell":
					s.log.WithField("reqType", req.Type).Info("User requesting shell access")
					if s.sys == nil {
						s.sys = os.NewSystem(s.user, viper.GetString("server.hostname"), s.fs, channel, 80, 24, s.log)
					}

					sh = os.NewShell(s.sys, s.src.String(), s.log.WithField("module", "shell"), quitSignal)

					// Create delay function if exists
					if viper.GetInt("server.processDelay") > 0 {
						sh.DelayFunc = func() {
							r := 500
							sleepTime := viper.GetInt("server.processDelay") - r + rand.Intn(2*r)
							time.Sleep(time.Millisecond * time.Duration(sleepTime))
						}
					}
					// Create hook for session logger (For recording session to UML/asciinema)
					var hook termlogger.LogHook
					if viper.GetString("server.sessionLogFmt") == "asciinema" {
						asciiLogParams := map[string]string{
							"TERM": s.term,
							"USER": s.user,
							"SRC":  s.src.String(),
						}
						hook, err = termlogger.NewAsciinemaHook(s.sys.Width(), s.sys.Height(),
							viper.GetString("asciinema.apiEndpoint"), viper.GetString("asciinema.apiKey"), asciiLogParams,
							fmt.Sprintf("logs/sessions/%v-%v.cast", s.user, termlogger.LogTimeFormat))

					} else if viper.GetString("server.sessionLogFmt") == "uml" {
						hook, err = termlogger.NewUMLHook(0, fmt.Sprintf("logs/sessions/%v-%v.ulm.log", s.user, time.Now().Format(logTimeFormat)))
					} else {
						log.Errorf("Session Log option %v not recognized", viper.GetString("server.sessionLogFmt"))
					}
					if err != nil {
						log.Errorf("Cannot create %v log file", viper.GetString("server.sessionLogFmt"))
					}
					// The need of a goroutine here is that PuTTY will wait for reply before acknowledge it enters shell mode
					go sh.HandleRequest(hook)
					req.Reply(true, nil)
				case "subsystem":
					subsys := string(req.Payload[4:])
					s.log.WithFields(log.Fields{
						"reqType":   req.Type,
						"subSystem": subsys,
					}).Infof("User requested subsystem %v", subsys)
					if subsys == "sftp" {
						sftpSrv := sftp.NewSftp(channel, s.fs,
							s.user, s.log.WithField("module", "sftp"), quitSignal)
						go sftpSrv.HandleRequest()
						req.Reply(true, nil)
					} else {
						req.Reply(false, nil)
					}
				case "window-change":
					s.log.WithField("reqType", req.Type).Info("User shell window size changed")
					if sh != nil {
						winChg := &winChgRequest{}
						if err := ssh.Unmarshal(req.Payload, winChg); err != nil {
							req.Reply(false, nil)
						}
						sh.SetSize(int(winChg.Width), int(winChg.Height))
					}
				case "exec":
					cmd := string(req.Payload[4:])
					s.log.WithFields(log.Fields{
						"reqType": req.Type,
						"cmd":     cmd,
					}).Info("User request remote exec")
					args := strings.Split(cmd, " ")
					var sys *os.System
					if s.sys == nil {
						sys = os.NewSystem(s.user, viper.GetString("server.hostname"), s.fs, channel, 80, 24, s.log)
					} else {
						sys = s.sys
					}
					if strings.HasPrefix(args[0], "scp") {
						scp := command.NewSCP(channel, s.fs, s.log.WithField("module", "scp"))
						go scp.Main(args[1:], quitSignal)
						req.Reply(true, nil)
						continue
					}
					n, err := sys.Exec(args[0], args[1:])
					if err != nil {
						channel.Write([]byte(fmt.Sprintf("%v: command not found\r\n", cmd)))
					}
					quitSignal <- n
					req.Reply(true, nil)
				default:
					s.log.WithField("reqType", req.Type).Infof("Unknown channel request type %v", req.Type)
				}
			case ret := <-quitSignal:
				s.log.Info("User closing channel")
				defer closeChannel(channel, ret)
				return
			}
		}
	}(requests, channel)
}

func (s *SSHSession) handleNewConn() {
	// Service the incoming Channel channel.
	for newChannel := range s.sshChan {
		s.log.WithField("chanType", newChannel.ChannelType()).Info("User created new session channel")
		switch newChannel.ChannelType() {
		case "direct-tcpip", "forwarded-tcpip":
			var treq tunnelRequest
			err := ssh.Unmarshal(newChannel.ExtraData(), &treq)
			if err != nil {
				s.log.WithError(err).Error("Cannot unmarshal port forwarding data")
				newChannel.Reject(ssh.UnknownChannelType, "Corrupt payload")
			}
			s.log.WithFields(log.Fields{
				"remoteHost": treq.RemoteHost,
				"remotePort": treq.RemotePort,
				"localHost":  treq.LocalHost,
				"localPort":  treq.LocalPort,
				"chanType":   newChannel.ChannelType(),
			}).Info("Trying to establish connection with port forwarding")
			if newChannel.ChannelType() == "forwarded-tcpip" {
				newChannel.Reject(ssh.Prohibited, "Port forwarding disabled")
				continue
			}
			var host string
			switch viper.GetString("server.portRedirection") {
			case "disable":
				newChannel.Reject(ssh.Prohibited, "Port forwarding disabled")
				continue
			case "map":
				portMap := viper.GetStringMap("server.portRedirectionMap")
				host = portMap[strconv.Itoa(int(treq.RemotePort))].(string)
			case "direct":
				host = fmt.Sprintf("%v:%v", treq.RemoteHost, treq.RemotePort)
			}
			if len(host) > 0 {
				ch, req, err := newChannel.Accept()
				if err != nil {
					newChannel.Reject(ssh.ResourceShortage, "Cannot create new channel")
				}
				go ssh.DiscardRequests(req)
				go func() {
					s.log.WithFields(log.Fields{
						"host": host,
					}).Infoln("Creating connection to remote server")
					conn, err := net.Dial("tcp", host)
					if err != nil {
						s.log.WithFields(log.Fields{
							"host": host,
						}).WithError(err).Error("Cannot create connection")
						newChannel.Reject(ssh.ConnectionFailed, "Cannot establish connection")
						return
					}
					go io.Copy(conn, ch)
					go io.Copy(ch, conn)
				}()
			} else {
				newChannel.Reject(ssh.ConnectionFailed, "Malformed channel request")
			}
		case "session":
			go s.handleNewSession(newChannel)
		default:
			newChannel.Reject(ssh.UnknownChannelType, "unknown channel type")
			s.log.WithField("chanType", newChannel.ChannelType()).Infof("Unknown channel type %v", newChannel.ChannelType())
			continue
		}
	}
}

func CreateSessionHandler(c <-chan net.Conn, sshConfig *ssh.ServerConfig, vfs afero.Fs) {
	for conn := range c {
		sshConfig.PasswordCallback = PasswordChallenge(viper.GetInt("server.maxTries"))
		sshSession, err := NewSSHSession(conn, sshConfig, vfs)
		clientIP, port, _ := net.SplitHostPort(conn.RemoteAddr().String())
		abuseipdb.CreateProfile(clientIP)
		abuseipdb.AddCategory(clientIP, abuseipdb.SSH, abuseipdb.Hacking)
		if err != nil {
			log.WithFields(log.Fields{
				"srcIP": clientIP,
				"port":  port,
			}).WithError(err).Error("Error establishing SSH connection")
		} else {
			sshSession.handleNewConn()
		}
		//conn.Close()
		ipConnCnt.DecCount(clientIP)
		abuseipdb.UploadReport(clientIP)
	}
}

func closeChannel(ch ssh.Channel, signal int) {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(signal))
	ch.SendRequest("exit-status", false, b)
	ch.Close()
}

func NewServer(configPath string, hostKey []byte) (s Server) {
	// Read banner
	bannerFile, err := ioutil.ReadFile(path.Join(configPath, viper.GetString("server.banner")))
	if err != nil {
		bannerFile = []byte{}
	}

	// Initalize VFS
	backupFS := afero.NewBasePathFs(afero.NewOsFs(), viper.GetString("virtualfs.savedFileDir"))
	zipfs, err := virtualfs.NewVirtualFS(path.Join(configPath, viper.GetString("virtualfs.imageFile")))
	if err != nil {
		log.Error("Cannot create virtual filesystem")
	}
	vfs := afero.NewCopyOnWriteFs(zipfs, backupFS)
	err = os.LoadUsers(path.Join(configPath, viper.GetString("virtualfs.uidMappingFile")))
	if err != nil {
		log.Errorf("Cannot load user mapping file %v", path.Join(configPath, viper.GetString("virtualfs.uidMappingFile")))
	}

	s = Server{
		&ssh.ServerConfig{
			PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
				clientIP, port, _ := net.SplitHostPort(c.RemoteAddr().String())
				log.WithFields(log.Fields{
					"user":              c.User(),
					"srcIP":             clientIP,
					"port":              port,
					"pubKeyType":        key.Type(),
					"pubKeyFingerprint": base64.StdEncoding.EncodeToString(key.Marshal()),
					"authMethod":        "publickey",
				}).Info("User trying to login with key")
				return nil, errors.New("Key rejected, revert to password login")
			},

			ServerVersion: viper.GetString("server.ident"),
			MaxAuthTries:  viper.GetInt("server.maxTries"),
			BannerCallback: func(c ssh.ConnMetadata) string {

				return string(bannerFile)
			},
		},
		vfs,
	}
	private, err := ssh.ParsePrivateKey(hostKey)
	if err != nil {
		log.WithError(err).Fatal("Failed to parse private key")
	}
	s.sshCfg.AddHostKey(private)

	return s
}

func PasswordChallenge(tries int) func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
	triesLeft := tries
	return func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
		clientIP, port, _ := net.SplitHostPort(c.RemoteAddr().String())
		log.WithFields(log.Fields{
			"user":       c.User(),
			"srcIP":      clientIP,
			"port":       port,
			"authMethod": "password",
			"password":   string(pass),
		}).Info("User trying to login with password")

		successPerm := &ssh.Permissions{
			Extensions: map[string]string{
				"permit-agent-forwarding": "yes",
			},
		}
		stpass, userExists := os.IsUserExist(c.User())
		if userExists && stpass == string(pass) {
			// Password match
			return successPerm, nil
		} else if userExists && (stpass != string(pass) || stpass == "*") || viper.GetBool("server.allowRandomUser") {
			if viper.GetBool("server.allowRetryLogin") {
				if triesLeft == 1 {
					return successPerm, nil
				}
				triesLeft--
			} else {
				return successPerm, nil
			}
		}
		time.Sleep(viper.GetDuration("server.retryDelay"))
		return nil, fmt.Errorf("password rejected for %q", c.User())
	}
}

func (sc Server) ListenAndServe() {
	connChan := make(chan net.Conn)
	// Create pool of workers to handle connections
	for i := 0; i < viper.GetInt("server.maxConnections"); i++ {
		go CreateSessionHandler(connChan, sc.sshCfg, sc.vfs)
	}

	listener, err := net.Listen("tcp", fmt.Sprintf("%v:%v", viper.GetString("server.addr"), viper.GetInt("server.port")))
	if err != nil {
		log.WithError(err).Fatal("Could not create listening socket")
	}
	defer listener.Close()

	for {
		nConn, err := listener.Accept()
		host, port, _ := net.SplitHostPort(nConn.RemoteAddr().String())
		log.WithFields(log.Fields{
			"srcIP": host,
			"port":  port,
		}).Info("Connection established")
		if err != nil {
			log.WithError(err).Error("Failed to accept incoming connection")
			continue
		}
		cnt := ipConnCnt.Read(host)
		if cnt >= viper.GetInt("server.maxConnPerHost") {
			nConn.Close()
			continue
		} else {
			ipConnCnt.IncCount(host)
		}
		tConn := netconn.NewThrottledConnection(nConn, viper.GetInt64("server.speed"), viper.GetDuration("server.timeout"))
		connChan <- tConn
	}
}
