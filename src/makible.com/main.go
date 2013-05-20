package main

import (
	"code.google.com/p/go.net/websocket"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

var (
	defServPort = "8080"
	uiDir       = "/ui/"
	// openBrowser = true
	// dbg         = false

	//  === [ DEBUGGING USE ]
	openBrowser = false
	dbg         = true

	devc, clientc         chan *Message
	devices               map[string]*Device
	workingDir            string
	launchBrowserArgs     []string
	deviceListenerRunning bool
)

func main() {
	log.Println("5DPrint starting...")
	runtime.GOMAXPROCS(2)

	devices = make(map[string]*Device)
	devc, clientc = make(chan *Message), make(chan *Message)

	initOSVars()
	go initDeviceListener()
	go initHttpServer()

	select {} // sleep forever
}

func initOSVars() {
	var err error

	switch runtime.GOOS {
	case "darwin":
		// workingDir          = "/Applications/5DPrint.app/Contents/MacOS"
		workingDir, err = os.Getwd()
		launchBrowserArgs = []string{"open"}
	case "windows":
		workingDir, err = os.Getwd()
		launchBrowserArgs = []string{"cmd", "/c", "start"}
	default:
		workingDir, err = os.Getwd()
		launchBrowserArgs = []string{"xdg-open"}
	}

	if err != nil {
		log.Println("initOSVars: ", err)
		os.Exit(1)
	}
}

func initDeviceListener() {
	log.Println("Listening for devices")

	for {
		deviceListenerRunning = true

		dn, err := getAttachedDevices(&devices)
		if err != nil {
			if !strings.HasSuffix(err.Error(), NSF) && !strings.HasSuffix(err.Error(), DNC) {
				if strings.HasSuffix(err.Error(), RM) {
					clientc <- &Message{
						Type:       "response",
						DeviceName: (strings.Split(err.Error(), " "))[0],
						Action:     "connection",
						Body:       "detached",
					}
				} else {
					//  [ TODO ]
					//  handle this better
					log.Println("initDeviceListener: ", err)
				}
			}
		}

		//  this means a new device was attached
		//  and someone should be notified
		if len(dn) > 1 {
			clientc <- &Message{
				Type:       "response",
				DeviceName: dn,
				Action:     "connection",
				Body:       "attached",
			}

			//  we'll just accept the one for now to free up some resources
			deviceListenerRunning = false
			log.Println("Device found, stopping listener")
			return
		}

		//  do a quick sleep so that we don't we don't ping
		//  the existing devices _too_ much
		time.Sleep(800 * time.Millisecond)
	}
}

func initHttpServer() {
	var ip string
	//  we need to get the hostname in order to get the IP
	host, err := os.Hostname()
	if err != nil {
		log.Println("initHttpServer: ", err)
		os.Exit(1)
	}

	//  list out the available IP's according to the hostname
	ipList, err := net.LookupIP(host)
	if err != nil {
		log.Println("initHttpServer: ", err)
		os.Exit(1)
	}

	//  check if an IPv4 is avialable and set to to 'localhost' if not
	//  we aren't going to work with IPv6 address at the moment, so
	//  ignore / exclude and just use the available IPv4 if ipList > 1
	if len(ipList) < 1 || (len(ipList) == 1 && strings.Contains(ipList[0].String(), ":")) {
		//  [ TODO ]
		//  double check and see if this is still valid when no network connection is available
		if len(ipList) == 1 && strings.Contains(ipList[0].String(), ":") {
			log.Println("[WARN] currently not supporting IPv6, defaulting to 'localhost'")
		}
		log.Println("[WARN] you will not be able to connect any external devices with a valid address")
		ip = "localhost"
	} else {
		if len(ipList) > 1 {
			for _, i := range ipList {
				if !strings.Contains(i.String(), ":") {
					ip = i.String()
				}
			}
		} else {
			ip = ipList[0].String()
		}
	}

	addr := ip + ":" + defServPort
	dir := workingDir + uiDir + "/default"

	//  [ TODO ]
	//  check .config to see if a specified UI is set,
	//  if not just use default

	fs := http.FileServer(http.Dir(dir))
	http.Handle("/favicon.ico", fs)
	http.Handle("/css/", fs)
	http.Handle("/js/", fs)
	http.Handle("/img/", fs)
	http.Handle("/fonts/", fs)

	http.Handle("/abs", websocket.Handler(clientWsHandler))
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			index := "/index.html"
			t, err := template.ParseFiles(dir + index)
			if err != nil {
				log.Fatal(fmt.Printf("initHttpServer: %v\n", err))
			}

			t.Execute(w, "")
			return
		}
		http.Error(w, "not found", 404)
	})

	go func() {
		url := "http://" + addr
		if httpWait(url) && openBrowser && launchBrowser(url) {
			log.Printf("[INFO] a browser window should open. If not, please visit %s\n", url)
		} else {
			log.Printf("[INFO] unable to open your browser. Please open and visit %s\n", url)
		}
	}()

	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}

func initJobQueue(dname string) {
	dev := devices[dname]
	lines := strings.Split(dev.FileData, "\n")

	log.Println("Starting job queue...")

	// for i, cmd := range lines {
	for _, cmd := range lines {
		//  [ TODO ]
		//  add a select here that will listen on a channel
		//  for an incoming interrupt and default to running
		//  the command
		if !strings.HasPrefix(cmd, ";") && cmd != "" {
			log.Println(cmd)

			//
			//  [ TODO ]
			//  This assumes device is a 3D printer. Need to make this more
			//  generalized, allowing for other types of devices
			if strings.HasPrefix(cmd, "M109") || strings.HasPrefix(cmd, "M190") {
				n, err := dev.IODevice.Write([]byte(cmd + dev.LineTerminator))
				if err != nil {
					log.Println(err)
					clientc <- &Message{
						Type:       "response",
						DeviceName: dev.Name,
						Action:     "error",
						Body: `{
                                        error: 'unable to complete job',
                                        body:  '` + err.Error() + `',
                                    }`,
					}
					return
				}

				if n < 1 {
					log.Println("unable to write to device")
					clientc <- &Message{
						Type:       "response",
						DeviceName: dev.Name,
						Action:     "error",
						Body: `{
                                        error:  'unable to write to device',
                                        action: 'job',
                                        body:   'job failed'
                                    }`,
					}
					return
				}

				pre, heatMsg := "B:", "waiting for bed to reach temp"
				if strings.HasPrefix(cmd, "M109") {
					pre, heatMsg = "T:", "waiting for hotend to reach temp"
				}

				log.Println(heatMsg)
				clientc <- responseMsg(dev.Name, "job", heatMsg)

				//  parse out the temp bit
				temp := cmd[strings.Index(cmd, "S")+1:]
				if strings.Contains(temp, " ") {
					temp = temp[:strings.Index(temp, " ")]
				}

				itemp, err := strconv.Atoi(temp)
				if err != nil {
					log.Println("error converting temp")
					return
				}

				//  give a high / low of about nth degrees
				offset := 2
				high, low := strconv.Itoa(itemp+offset), strconv.Itoa(itemp-offset)

				for {
					log.Println("debug: requesting temp")

					buf := make([]byte, 255)
					n, err := dev.IODevice.Read(buf)
					if n < 1 || err != nil {
						log.Printf("unable to get valid response from device: %d\n", n)
						break
					}

					val := string(buf[:n])

					log.Println(val)
					clientc <- responseMsg(dev.Name, "status", val)

					if strings.Contains(val, pre+temp) || strings.Contains(val, pre+high) || strings.Contains(val, pre+low) {
						break
					}
				}
			} else {

				resp, err := dev.LobCommand(cmd + dev.LineTerminator)
				if err != nil {
					if checkConnError(err.Error(), dev.Name) {
						dev.JobStatus = IDLE
						delete(devices, dev.Name)
						if !deviceListenerRunning {
							go initDeviceListener()
						}

						return
					}
				}

				log.Println(resp)
				clientc <- responseMsg(dev.Name, "job", resp)

				// if (i % 10) == 0 {
				// 	resp, err := dev.Do("status", "")
				// 	if err != nil {
				// 		if checkConnError(err.Error(), dev.Name) {
				// 			dev.JobStatus = IDLE
				// 			delete(devices, dev.Name)
				// 			if !deviceListenerRunning {
				// 				go initDeviceListener()
				// 			}

				// 			return
				// 		}
				// 	}

				// 	log.Println(resp)
				// }
			}
		}
	}

	clientc <- &Message{
		Type:       "response",
		DeviceName: dev.Name,
		Action:     "job",
		Body:       "complete",
	}

	dev.JobStatus = IDLE
	dev.FileData = ""
	dev.FileName = ""
}

func clientWsHandler(ws *websocket.Conn) {
	go func() {
		enc := json.NewEncoder(ws)
		for msg := range clientc {
			if err := enc.Encode(msg); err != nil {
				log.Println("clientWsHandler: ", err)
				return
			}
		}
	}()

	dec := json.NewDecoder(ws)
	for {
		var msg Message
		if err := dec.Decode(&msg); err != nil && err != io.EOF {
			log.Println("clientWsHandler: ", err)
			return
		}

		if msg.Type == "" && msg.Action == "" {
			ws.Close()
			return
		}

		if msg.Action != "connection" && (devices == nil || len(devices) < 1) {
			clientc <- &Message{
				Type:       "response",
				DeviceName: msg.DeviceName,
				Action:     "error",
				Body: `{
                                error:  'invalid device name',
                                action: '` + msg.Action + `',
                                body:   '` + msg.Body + `',
                            }`,
			}

			continue
		}

		dev := devices[msg.DeviceName]
		switch msg.Action {
		case "connection":
			//
			//  if the message is a connection request go ahead
			//  and inform the UI else start the device listener
			if devices != nil && len(devices) > 0 {
				found := false

				for dn, _ := range devices {
					clientc <- &Message{
						Type:       "response",
						DeviceName: dn,
						Action:     "connection",
						Body:       "attached",
					}

					found = true
					break
				}

				if !found {
					clientc <- &Message{
						Type:       "response",
						DeviceName: "",
						Action:     "error",
						Body: `{
                                        error:  'no devices avialable',
                                        action: '` + msg.Action + `',
                                        body:   '` + msg.Body + `',
                                    }`,
					}

					if !deviceListenerRunning {
						go initDeviceListener()
					}
				}
			} else {
				//  if the list is empty and the UI is requesting a device,
				//  we should probably be looking for a device
				if !deviceListenerRunning {
					go initDeviceListener()
				}
			}
		case "job":
			if dev.JobStatus == RUNNING {
				clientc <- &Message{
					Type:       "response",
					DeviceName: dev.Name,
					Action:     "error",
					Body: `{
                                    error:  'unable to run multiple jobs on a single device',
                                    action: '` + msg.Action + `',
                                    body:   '` + msg.Body + `',
                                }`,
				}
			} else {
				if len(strings.Split(dev.FileData, "\n")) > 1 {
					dev.JobStatus = RUNNING
					go initJobQueue(dev.Name)
				} else {
					clientc <- &Message{
						Type:       "response",
						DeviceName: msg.DeviceName,
						Action:     "error",
						Body: `{
                                        error:  'invalid job file',
                                        action: '` + msg.Action + `',
                                        body:   '` + msg.Body + `',
                                    }`,
					}

				}
			}
		case "interrupt":
			//  [ TODO ]

		default:
			r, err := dev.Do(msg.Action, msg.Body)
			if err != nil {
				if checkConnError(err.Error(), msg.DeviceName) {
					delete(devices, msg.DeviceName)
					if !deviceListenerRunning {
						go initDeviceListener()
					}
				} else {
					log.Println("dev.Do: ", err)
				}
			}

			if r != nil {
				clientc <- r
			} //  send the response even with an error
		}
	}
}

//  wait a bit for the web server to start
func httpWait(url string) bool {
	tries := 20
	for tries > 0 {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return true
		}
		time.Sleep(100 * time.Millisecond)
		tries--
	}
	return false
}

func launchBrowser(url string) bool {
	cmd := exec.Command(launchBrowserArgs[0], append(launchBrowserArgs[1:], url)...)
	return cmd.Start() == nil
}

func checkConnError(err string, dn string) bool {
	//
	//  This usually means the device was detached.
	//  We will update the client / UI, clean up values and exit
	if strings.HasSuffix(err, NSF) || strings.HasSuffix(err, DNC) {
		clientc <- &Message{
			Type:       "response",
			DeviceName: dn,
			Action:     "error",
			Body:       "{ error: 'device not available' }",
		}
		return true
	}
	return false
}
