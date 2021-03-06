package main

import (
  "fmt"
  "github.com/digitalocean/godo"
  "golang.org/x/oauth2"
  "log"
  "io/ioutil"
  "os/user"
  "strconv"
  "strings"
  "github.com/dropbox/godropbox/errors"
  "golang.org/x/crypto/ssh"
  "bytes"
  "flag"
)


// FIXME: be able to configure the used SSH key
func main() {
  fmt.Println("running digital ocean setup script..")

  pat, err := ReadTokenFromConfigFile()
  if err != nil {
    log.Println(err)
    return
  }

  tokenSource := &TokenSource{
    AccessToken: pat,
  }
  oauthClient := oauth2.NewClient(oauth2.NoContext, tokenSource)
  client := godo.NewClient(oauthClient)

  cmd := flag.String("c", "list", "command: list ; delete ; create ; install")

  slaveCount := flag.Int("slaves", 5, "slave count argument used upon creation")

  procType := flag.String("type", "", "which type of nodes to install (slave/master). unspecified means all")
  procArgs := flag.String("args", "", "arguments to that specific process")

  flag.Parse()

  droplets, err := DropletList(client)
  if err != nil {
    log.Println(err)
    return
  }

  switch (*cmd) {
  case "list":
    log.Println(droplets)
  case "delete":
    err = RemoveAllDroplets(client)
  case "create":
    err = createMasterSlaveDroplets(client, *slaveCount)
  case "install":
    runMaster := false
    runSlaves := false
    switch *procType {
    case "": runMaster = true ; runSlaves = true
    case "slave": runSlaves = true
    case "master": runMaster = true
    default:
      log.Fatal("unknown install type " + *procType)
      return
    }
    master, slaves := GetTentacularDroplets(droplets)
    err = RunTentacularOnDroplets(master, slaves, runMaster, runSlaves, *procArgs)
  default:
    log.Fatal("unknown command " + *cmd)
    return
  }

  if err != nil {
    log.Println(err)
    return
  }
}

func createMasterSlaveDroplets(client *godo.Client, slaveCount int) (err error) {

  sshKeys := []godo.DropletCreateSSHKey{godo.DropletCreateSSHKey{Fingerprint: "9e:6a:0b:3d:0a:d1:af:c6:7f:d3:00:aa:b3:a1:ed:dc"}}

  log.Println("creating master... ")
  _, err = createSmallDroplet(client, "master", sshKeys)
  if err != nil { return }

  for i := 0; i < slaveCount; i++ {
    slaveName := "slave" + strconv.Itoa(i)
    log.Println("creating slave with name " + slaveName)
    _, err = createSmallDroplet(client, slaveName, sshKeys)
    if err != nil { return }
  }
  return
}

func createSmallDroplet(client *godo.Client, dropletName string, sshKeys []godo.DropletCreateSSHKey) (*godo.Droplet, error) {

  // Docker 1.10.1 on 14.04 in San Francisco
  createRequest := &godo.DropletCreateRequest{
    Name:   dropletName,
    Region: "sfo1",
    Size:   "512mb",
    PrivateNetworking: true,
    SSHKeys: sshKeys,
    Image: godo.DropletCreateImage{
      Slug: "docker",
    },
  }

  newDroplet, _, err := client.Droplets.Create(createRequest)
  return newDroplet, err
}

func RemoveAllDroplets(client *godo.Client) error {
  log.Println("deleting all droplets...")
  droplets, err := DropletList(client)
  if err != nil {
    return err
  }

  for _, droplet := range droplets {
    _, err := client.Droplets.Delete(droplet.ID)

    if (err != nil) {
      return err
    }
  }
  return nil
}

type TokenSource struct {
AccessToken string
}

func DropletList(client *godo.Client) ([]godo.Droplet, error) {
  // create a list to hold our droplets
  list := []godo.Droplet{}

  // create options. initially, these will be blank
  opt := &godo.ListOptions{}
  for {
    droplets, resp, err := client.Droplets.List(opt)
    if err != nil {
      return nil, err
    }

    // append the current page's droplets to our list
    for _, d := range droplets {
      list = append(list, d)
    }

    // if we are at the last page, break out the for loop
    if resp.Links == nil || resp.Links.IsLastPage() {
      break
    }

    page, err := resp.Links.CurrentPage()
    if err != nil {
      return nil, err
    }

    // set the page we want for the next request
    opt.Page = page + 1
  }

  return list, nil
}

func ReadTokenFromConfigFile() (token string, err error) {
  usr, err := user.Current()
  if err != nil { return }

  bytes, err := ioutil.ReadFile(usr.HomeDir + "/.digitalOceanToken")
  if err != nil { return }

  return string(bytes), err
}

func (t *TokenSource) Token() (*oauth2.Token, error) {
  token := &oauth2.Token{
    AccessToken: t.AccessToken,
  }
  return token, nil
}


func GetTentacularDroplets(droplets []godo.Droplet) (master *godo.Droplet, slaves []godo.Droplet) {
  slaves = []godo.Droplet{}
  for _, droplet := range droplets {
    if IsMasterDroplet(&droplet) {
      current := droplet
      master = &current
    } else if IsSlaveDroplet(&droplet) {
      slaves = append(slaves, droplet)
    }
  }

  return master, slaves
}

func RunTentacularOnDroplets(master *godo.Droplet, slaves []godo.Droplet,
                             runMaster, runSlaves bool, args string) (err error) {

  nodeCount := 0
  if runMaster { nodeCount++ }
  if runSlaves { nodeCount += len(slaves) }
  doneChan := make(chan error, nodeCount)

  if runMaster {
    go func() {
      err = RunTentacularMaster(master, args)
      doneChan <- err
    }()
  }

  if runSlaves {
    if len(slaves) == 0 {
      return errors.New("No slave nodes available.")
    }

    masterPrivAddr, err := master.PrivateIPv4()
    if err != nil { err = errors.Wrap(err, "") ; return err }

    slaveCommand := fmt.Sprintf(RUN_PROXY_SLAVE, masterPrivAddr)

    log.Print("Running command on slaves: " + slaveCommand)

    for _, slave := range slaves {

      slaveAddr, err := slave.PublicIPv4()
      if err != nil {
        fmt.Errorf("slave address could not be obtained. ignoring")
        continue
      }

      go func() {
        log.Println("running slave proxy at " + slaveAddr)
        reString, err := RunRemoteCommand(slaveAddr, setupCmd(slaveCommand, args))
        log.Println("slave terminated with output " + reString)
        if err != nil {
          log.Println("slave terminated with error.")
          log.Println(err)
        }
        doneChan <- err
      }()
    }
  }

  log.Println("waiting for procs to finish..")
  for i := 0; i < nodeCount; i++ {
    <- doneChan
  }

  return nil
}

func RunTentacularMaster(master *godo.Droplet, args string) error {
  if master == nil {
    return errors.New("Missing master node.")
  }
  masterPubAddr, err := master.PublicIPv4()
  if err != nil { err = errors.Wrap(err, "") ; return err }

  masterCmd := setupCmd(RUN_PROXY_MASTER, args)
  log.Print("Running command on master: " + masterCmd)
  log.Println("Running master proxy at " + masterPubAddr)
  
  reString, err := RunRemoteCommand(masterPubAddr, masterCmd)
  log.Println("master terminated with output " + reString)
  if err != nil {
    log.Println("master terminated with error.")
    log.Println(err)
  }
  return nil
}

func IsMasterDroplet(droplet *godo.Droplet) bool {
  return strings.HasPrefix(droplet.Name, "master")
}

func IsSlaveDroplet(droplet *godo.Droplet) bool {
  return strings.HasPrefix(droplet.Name, "slave")
}

func setupCmd(cmd string, args string) string {
  return fmt.Sprintf("(%s %s); sleep 5", cmd, args)
}

// on a droplet assuming root user and ssh key at id_rsa.pub deployed
func RunRemoteCommand(addr string, command string) (s string, err error) {

  usr, err := user.Current()
  if err != nil { err = errors.Wrap(err, "") ; return }

  authMethod, err := PublicKeyFile(usr.HomeDir + "/.ssh/id_rsa")
  if err != nil { err = errors.Wrap(err, "") ; return }

  sshConfig := &ssh.ClientConfig{
    User: "root",
    Auth: []ssh.AuthMethod{authMethod},
  }

  conn, err := ssh.Dial("tcp", addr + ":22", sshConfig)
  if err != nil { err = errors.Wrap(err, "ssh dial failed.") ; return }

  defer conn.Close()

  session, err := conn.NewSession()
  if err != nil { err = errors.Wrap(err, "session failed."); return }

  defer session.Close()

  var stdoutBuf bytes.Buffer
  session.Stdout = &stdoutBuf
  err = session.Run(command)
  if err != nil { err = errors.Wrap(err, "cmd failed."); return }

  return stdoutBuf.String(), nil
}

func PublicKeyFile(file string) (auth ssh.AuthMethod, err error) {
  buffer, err := ioutil.ReadFile(file)
  if err != nil { err = errors.Wrap(err, "") ; return }

  key, err := ssh.ParsePrivateKey(buffer)
  if err != nil { err = errors.Wrap(err, "") ; return }

  return ssh.PublicKeys(key), nil
}

const RUN_PROXY_MASTER = DOCKER_REMOVE_ALL_CONTAINERS + "docker run -p 8080:8080 -p 6666:6666 --name master --rm danoctavian/tentacular /go/bin/app --type=master"
const RUN_PROXY_SLAVE = DOCKER_REMOVE_ALL_CONTAINERS + "docker run -p 8080:8080 --name slave --rm danoctavian/tentacular /go/bin/app --masterurl=\"http://%s:6666\""

const DOCKER_REMOVE_ALL_CONTAINERS = "docker rm --force `docker ps -qa`;"