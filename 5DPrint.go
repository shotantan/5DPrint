package main

import (
    "code.google.com/p/go.net/websocket"
    "device"
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
    "strings"
    "time"
)

var (
    defServPort = "8080"
    uiDir       = "/ui/"
    // openBrowser = true
    // dbg = false

    //  [ TODO ]
    //  break out into .conf flag
    // defServPort = "8081"
    openBrowser = false
    dbg = true

    devc, clientc           chan *device.Message
    devices                 map[string] *device.Device
    workingDir              string
    launchBrowserArgs       []string
    deviceListenerRunning   bool
)

func main() {
    log.Println("[INFO] 5DPrint starting...")
    runtime.GOMAXPROCS(2)   //  increasing the count for background processes

    devices              = make(map[string] *device.Device)
    devc, clientc        = make(chan *device.Message), make(chan *device.Message)

    //  init OS specific variables
    initOSVars()
    go initDeviceListener()
    initHttpServer()
}

func initOSVars() {
    var err error

    switch runtime.GOOS {
    case "darwin":
        // workingDir          = "/Applications/5DPrint.app/Contents/MacOS"
        workingDir, err     = os.Getwd()
        launchBrowserArgs   = []string{"open"}
    case "windows":
        workingDir, err     = os.Getwd()
        launchBrowserArgs   = []string{"cmd", "/c", "start"}
    default:
        workingDir, err     = os.Getwd()
        launchBrowserArgs   = []string{"xdg-open"}
    }

    if err != nil {
        log.Println("[ERROR] unable to get a valid working directory: ", err)
        os.Exit(1)
    }
}

//
//  start the loop that will check for valid devices
//  attached and update the list accordingly
func initDeviceListener() {
    for {
        if !deviceListenerRunning { deviceListenerRunning = true }

        dn, err := device.GetAttachedDevices(&devices)
        if err != nil {
            if !strings.HasSuffix(err.Error(), device.NSF) && !strings.HasSuffix(err.Error(), device.DNC) {
                if strings.HasSuffix(err.Error(), device.RM) {
                    //  notify device detached
                    clientc <- &device.Message {
                        Type:   "response",
                        Device: (strings.Split(err.Error(), " "))[0],
                        Action: "connection",
                        Body:   "detached",
                    }
                }else {
                    //
                    //  [ TODO ] 
                    //  handle this better but for now
                    //  just display the error
                    log.Println("[ERROR] device check: ", err)

                }
            }
        }

        //  this means a new device was attached
        //  and someone should be notified
        if len(dn) > 1 {
            clientc <- &device.Message {
                Type:   "response",
                Device: dn,
                Action: "connection",
                Body:   "attached",
            }

            //  we'll just accept the one for now to free up some resources
            deviceListenerRunning = false
            return
        }

        //  do a quick sleep so that we don't we don't ping
        //  the existing devices _too_ much
        time.Sleep(800 * time.Millisecond) 
    }
}

func initJobQueue(dev *device.Device) {
    go func() {
        done, loopCounter := false, 0

        for !done {
            cmd := dev.JobQueue[0]
            if dev.JobRunning && !dev.JobPaused && len(dev.JobQueue) > 1 {
                dev.JobQueue = dev.JobQueue[1:len(dev.JobQueue)-1]  //  pop the zero item off

                //
                //  Do not send over comments or empty commands.
                //  Empty lines are possible depending on the slicer used
                if !strings.HasPrefix(cmd, ";") && cmd != "" {
                    //  debug for now; hide later and possible reply back to the UI that the cmd
                    //  has been shipped to the device
                    log.Println("[INFO] JobQueue cmd: \n", cmd)

                    //  wash and tag
                    cmd = strings.TrimLeft(cmd, " ")
                    cmd += device.FWLINETERMINATOR
                    resp, err := dev.LobCommand(cmd)
                    if err != nil {
                        log.Println("[ERROR] dev.JobQueue processing for device ", dev.Name, err)

                        //  
                        //  This usually means the device was detached.
                        //  We will update the client / UI, clean up values and exit
                        if strings.HasSuffix(err.Error(), device.NSF) || strings.HasSuffix(err.Error(), device.DNC) {
                            clientc <-&device.Message {
                                Type:   "response",
                                Device: dev.Name,
                                Action: "error",
                                Body:   `{
                                            error:   'device not available',
                                            command: '` + cmd + `',
                                        }`,
                            }

                            dev.JobQueue   = make([]string, 1)
                            dev.JobRunning = false
                            delete(devices, dev.Name)
                            go initDeviceListener()

                            done = true
                            continue
                        }
                    }

                    //  debug info
                    log.Println("[INFO] JobQueue response: ", resp)
                    clientc <-dev.ResponseMsg("job", resp)

                    //  
                    //  === [ HACK ]
                    //  this is assuming we're on a 3D
                    //  printer and doesn't generalize
                    //  for all devices
                    if strings.HasPrefix(cmd, "M109") || strings.HasPrefix(cmd, "M190") {
                        pre, heatMsg := "B:", "waiting for bed to reach temp"
                        if strings.HasPrefix(cmd, "M109") {
                            pre, heatMsg = "T:", "waiting for hotend to reach temp"
                        }

                        log.Println("[INFO] ", heatMsg)
                        clientc <-dev.ResponseMsg("job", heatMsg)

                        //  parse out the temp bit
                        temp := cmd[strings.Index(cmd, "S")+1:]
                        if strings.Contains(temp, " ") {
                            temp = temp[:strings.Index(temp, " ")]
                        }

                        for {
                            buf := make([]byte, 255)
                            n, err := dev.IODevice.Read(buf)
                            if n < 1 || err != nil {
                                log.Printf("[ERROR] looks like the device didn't respond properly: %d\n", n)
                                break
                            }

                            val := string(buf[:n])
                            log.Println("[INFO] ", val)
                            clientc <- dev.ResponseMsg("job", val)

                            if strings.Contains(pre + temp, val) {
                                log.Println("[DEBUG] temp: ", val)
                                break
                            }
                        }
                    }


                    loopCounter++
                    if loopCounter % 8 == 0 {
                        var (
                            r *device.Message
                            e error
                        )

                        r, e = dev.Do("status", "")
                        if e != nil { log.Println("[WARN] JobQueue: unable to get status info: ", e) }
                        if r != nil { 
                            log.Println("[INFO] JobQueue - status response: ", r.Body)
                            clientc <-r 
                        }

                        //  show RAM info
                        r, e = dev.Do("console", "M603")
                        if e != nil { log.Println("[WARN] JobQueue: unable to get status info: ", e) }
                        if r != nil { 
                            log.Println("[INFO] JobQueue - RAM info response: ", r.Body)
                            clientc <- r 
                        }
                    }
                }
            } else {
                done = true
            }
        }

        log.Println("[INFO] JobQueue cleaning up and exiting")
        log.Println("[DEBUG] JobRunning ", dev.JobRunning)
        log.Println("[DEBUG] JobPaused ", dev.JobPaused)
        log.Println("[DEBUG] len(dev.JobQueue)", len(dev.JobQueue))
        log.Println("[DEBUG]")

        //  cleanup
        dev.JobRunning = false
        dev.JobQueue   = make([]string, 1)

    }()
}

func initHttpServer() {
    var ip string
    //  we need to get the hostname in order to get the IP
    host, err := os.Hostname()
    if err != nil {
        log.Println("[ERROR] unable to get app server address: ", err)
        os.Exit(1)
    }

    //  list out the available IP's according to the hostname
    ipList, err := net.LookupIP(host)
    if err != nil {
        log.Println("[ERROR] unable to get app server address: ", err)
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
    dir  := workingDir + uiDir + "/default"

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
            t, err := template.ParseFiles(dir + "/index.html")
            if err != nil {
                log.Fatal(fmt.Printf("[ERROR] unable to reneder default UI: %v\n", err))
            }
            t.Execute(w, "")
            return
        }
        http.Error(w, "not found", 404)
    })

    go func() {
        url := "http://" + addr
        if wait(url) && openBrowser && launchBrowser(url) {
            log.Printf("[INFO] a browser window should open. If not, please visit %s\n", url)
        } else {
            log.Printf("[INFO] unable to open your browser. Please open and visit %s\n", url)
        }
    }()
    log.Fatal(http.ListenAndServe(addr, nil))
}

//  
//  === [ HELPERS ]
//  

//  wait a bit for the web server to start
func wait(url string) bool {
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

func clientWsHandler(c *websocket.Conn) {
    //  [ TODO ]
    //  do we need a check in each of these routines
    //  that will return when the channels are closed?
    //  will this holdq open memory after the application
    //  hask "shutdown"?
    go func() {
        enc := json.NewEncoder(c)
        for m := range clientc {
            if err := enc.Encode(m); err != nil {
                log.Println("[ERROR] clientc channel read: ", err)
                return
            }
        }
    }()

    dec := json.NewDecoder(c)
    for {
        var msg device.Message
        if err := dec.Decode(&msg); err != nil  && err != io.EOF {
            log.Println("[ERROR] dec.Decode: ", err)
            return
        }

        if msg.Action != "connection" {
            if devices != nil && len(devices) > 0 && devices[msg.Device] != nil {
                dev := devices[msg.Device]
                if dev.JobRunning && msg.Action == "job" {
                    clientc <-&device.Message {
                        Type:   "response",
                        Device: dev.Name,
                        Action: "error",
                        Body:   `{
                                    error:  'unable to run multiple jobs on single device',
                                    action: '` + msg.Action + `',
                                    body:   '` + msg.Body + `',
                                }`,
                    }
                } else {
                    if msg.Action == "job" && !dev.JobRunning {
                        //  load up the job queue and let it run
                        lines := strings.Split(dev.GCode.Data, "\n")
                        if len(lines) > 1 {
                            dev.JobQueue = make([]string, 1)
                            for _, line := range lines {
                                dev.JobQueue = append(dev.JobQueue, line)
                            }

                            dev.JobQueue    = dev.JobQueue[1:len(dev.JobQueue)-1] //   pop the empty item off
                            dev.JobRunning  = true
                            initJobQueue(dev)
                        } else {
                            clientc <-&device.Message {
                                Type:   "response",
                                Device: msg.Device,
                                Action: "error",
                                Body:   `{
                                            error:   'invalid job file',
                                            action:  '` + msg.Action + `',
                                            body:    '` + msg.Body + `',
                                        }`,
                            }
                        }
                    } else {
                        if !dev.JobRunning {
                            r, err := dev.Do(msg.Action, msg.Body)
                            if err != nil {
                                if strings.HasSuffix(err.Error(), device.NSF) || strings.HasSuffix(err.Error(), device.DNC) {
                                    clientc <-&device.Message {
                                        Type:   "response",
                                        Device: msg.Device,
                                        Action: "error",
                                        Body:   `{
                                                    error:   'device not available',
                                                    action:  '` + msg.Action + `',
                                                    body:    '` + msg.Body + `',
                                                }`,
                                    }
                                    delete(devices, msg.Device)
                                    go initDeviceListener()
                                } else {
                                    log.Println("[ERROR] unable to complete action: ", err)
                                }
                            }
                            if r != nil { clientc <-r }    //  send the response even with an error
                        } else {
                            if msg.Action == "status" {
                                r, err := dev.Do(msg.Action, msg.Body)
                                if err != nil {
                                    if strings.HasSuffix(err.Error(), device.NSF) || strings.HasSuffix(err.Error(), device.DNC) {
                                        clientc <-&device.Message {
                                            Type:   "response",
                                            Device: msg.Device,
                                            Action: "error",
                                            Body:   `{
                                                        error:   'device not available',
                                                        action:  '` + msg.Action + `',
                                                        body:    '` + msg.Body + `',
                                                    }`,
                                        }
                                        delete(devices, msg.Device)
                                        go initDeviceListener()
                                    } else {
                                        log.Println("[ERROR] unable to complete action: ", err)
                                    }
                                }
                                if r != nil { clientc <-r }    //  send the response even with an error
                            } else if msg.Action == "resume" && dev.JobPaused {
                                //
                                //  Shift from HoldQueue to JobQueue
                                dev.JobQueue  = make([]string, len(dev.HoldQueue))
                                copy(dev.JobQueue, dev.HoldQueue)

                                dev.HoldQueue = make([]string, 1)
                                dev.JobPaused = false;
                                initJobQueue(dev)

                            } else if msg.Action == "interrupt" {
                                //
                                //  Shift from JobQueue to HoldQueue
                                dev.HoldQueue = make([]string, len(dev.JobQueue))
                                copy(dev.HoldQueue, dev.JobQueue)

                                dev.JobQueue  = make([]string, 1)
                                dev.JobPaused = true;
                            }
                        }
                    }
                }
            } else {
                clientc <-&device.Message {
                    Type:   "response",
                    Device: msg.Device,
                    Action: "error",
                    Body:   `{
                                error:  'invalid device provided',
                                action: '` + msg.Action + `',
                                body:   '` + msg.Body + `',
                            }`,
                }
            }
        } else {
            //  check device list, if no list, send back empty connection
            //  else notify there is a device available
            if devices != nil && len(devices) > 0 {
                for dn, _ := range devices {
                    clientc <-&device.Message {
                        Type:   "response",
                        Device: dn,
                        Action: "connection",
                        Body:   "attached",
                    }
                    break
                }
            } else {
                if !deviceListenerRunning { go initDeviceListener() }
            }
        }
    }
}
