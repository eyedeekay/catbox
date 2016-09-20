/*
 * IRC daemon.
 */

package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"summercat.com/irc"
)

// Client holds state about a single client connection.
type Client struct {
	Conn irc.Conn

	WriteChan chan irc.Message

	// A unique id.
	ID uint64

	IP net.IP

	// Whether it completed connection registration.
	Registered bool

	// Not canonicalized
	Nick string

	User string

	RealName string

	// Channel name (canonicalized) to Channel.
	Channels map[string]*Channel

	Server *Server

	// The last time we heard from the client.
	LastActivityTime time.Time

	// The last time we sent the client a PING.
	LastPingTime time.Time

	// User modes
	Modes map[byte]struct{}
}

// Channel holds everything to do with a channel.
type Channel struct {
	// Canonicalized.
	Name string

	// Client id to Client.
	Members map[uint64]*Client

	// TODO: Modes

	// TODO: Topic
}

// Server holds the state for a server.
// I put everything global to a server in an instance of struct rather than
// have global variables.
type Server struct {
	Config irc.Config

	// Client id to Client.
	Clients map[uint64]*Client

	// Canoncalized nickname to Client.
	// The reason I have this as well as clients is to track unregistered
	// clients.
	Nicks map[string]*Client

	// Channel name (canonicalized) to Channel.
	Channels map[string]*Channel

	// When we close this channel, this indicates that we're shutting down.
	// Other goroutines can check if this channel is closed.
	ShutdownChan chan struct{}

	Listener net.Listener

	// WaitGroup to ensure all goroutines clean up before we end.
	WG sync.WaitGroup

	// Period of time to wait before waking server up (maximum).
	WakeupTime time.Duration

	// Period of time a client can be idle before we send it a PING.
	PingTime time.Duration

	// Period of time a client can be idle before we consider it dead.
	DeadTime time.Duration

	// Oper name to password.
	Opers map[string]string
}

// ClientMessage holds a message and the client it originated from.
type ClientMessage struct {
	Client  *Client
	Message irc.Message
}

// Args are command line arguments.
type Args struct {
	ConfigFile string
}

// 9 from RFC
const maxNickLength = 9

// 50 from RFC
const maxChannelLength = 50

func main() {
	log.SetFlags(0)

	args, err := getArgs()
	if err != nil {
		log.Fatal(err)
	}

	config, err := irc.LoadConfig(args.ConfigFile)
	if err != nil {
		log.Fatal(err)
	}

	server, err := newServer(config)
	if err != nil {
		log.Fatal(err)
	}

	err = server.start()
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("Server shutdown cleanly.")
}

func getArgs() (Args, error) {
	configFile := flag.String("config", "", "Configuration file.")

	flag.Parse()

	if len(*configFile) == 0 {
		flag.PrintDefaults()
		return Args{}, fmt.Errorf("You must provie a configuration file.")
	}

	return Args{ConfigFile: *configFile}, nil
}

func newServer(config irc.Config) (*Server, error) {
	s := Server{
		Config: config,
	}

	err := s.checkAndParseConfig()
	if err != nil {
		return nil, fmt.Errorf("Configuration problem: %s", err)
	}

	s.Clients = map[uint64]*Client{}
	s.Nicks = map[string]*Client{}
	s.Channels = map[string]*Channel{}

	s.ShutdownChan = make(chan struct{})

	return &s, nil
}

// checkAndParseConfig checks configuration keys are present and in an
// acceptable format.
// We parse some values into alternate representations.
func (s *Server) checkAndParseConfig() error {
	requiredKeys := []string{
		"listen-host",
		"listen-port",
		"server-name",
		"server-info",
		"version",
		"created-date",
		"motd",
		"wakeup-time",
		"ping-time",
		"dead-time",
		"opers-config",
	}

	// TODO: Check format of each

	for _, key := range requiredKeys {
		v, exists := s.Config[key]
		if !exists {
			return fmt.Errorf("Missing required key: %s", key)
		}

		if len(v) == 0 {
			return fmt.Errorf("Configuration value is blank: %s", key)
		}
	}

	var err error

	s.WakeupTime, err = time.ParseDuration(s.Config["wakeup-time"])
	if err != nil {
		return fmt.Errorf("Wakeup time is in invalid format: %s", err)
	}

	s.PingTime, err = time.ParseDuration(s.Config["ping-time"])
	if err != nil {
		return fmt.Errorf("Ping time is in invalid format: %s", err)
	}

	s.DeadTime, err = time.ParseDuration(s.Config["dead-time"])
	if err != nil {
		return fmt.Errorf("Dead time is in invalid format: %s", err)
	}

	opers, err := irc.LoadConfig(s.Config["opers-config"])
	if err != nil {
		return fmt.Errorf("Unable to load opers config: %s", err)
	}

	s.Opers = opers

	return nil
}

// start starts up the server.
//
// We open the TCP port, open our channels, and then act based on messages on
// the channels.
func (s *Server) start() error {
	// TODO: TLS
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%s", s.Config["listen-host"],
		s.Config["listen-port"]))
	if err != nil {
		return fmt.Errorf("Unable to listen: %s", err)
	}
	s.Listener = ln

	// We hear about new client connections on this channel.
	newClientChan := make(chan *Client, 100)

	// We hear messages from clients on this channel.
	messageServerChan := make(chan ClientMessage, 100)

	// We hear messages when client read/write fails so we can clean up the
	// client.
	// It's also useful to be able to know immediately and inform the client if
	// we're going to decide they are getting cut off (e.g., malformed message).
	deadClientChan := make(chan *Client, 100)

	s.WG.Add(1)
	go s.acceptConnections(newClientChan, messageServerChan, deadClientChan)

	// Alarm is a goroutine to wake up this one periodically so we can do things
	// like ping clients.
	fromAlarmChan := make(chan struct{})

	s.WG.Add(1)
	go s.alarm(fromAlarmChan)

MessageLoop:
	for {
		select {
		case client := <-newClientChan:
			log.Printf("New client connection: %s", client)
			s.Clients[client.ID] = client
			client.LastActivityTime = time.Now()

		case clientMessage := <-messageServerChan:
			log.Printf("Client %s: Message: %s", clientMessage.Client,
				clientMessage.Message)

			// This could be from a client that disconnected. Ignore it if so.
			_, exists := s.Clients[clientMessage.Client.ID]
			if exists {
				s.handleMessage(clientMessage.Client, clientMessage.Message)
			}

		case client := <-deadClientChan:
			_, exists := s.Clients[client.ID]
			if exists {
				log.Printf("Client %s died.", client)
				client.quit("I/O error")
			}

		case <-fromAlarmChan:
			s.checkAndPingClients()

		case <-s.ShutdownChan:
			break MessageLoop
		}
	}

	// We're shutting down. Drain all channels. We want goroutines that send on
	// them to not be blocked.
	for range newClientChan {
	}
	for range fromAlarmChan {
	}

	// We don't need to drain messageServerChan or deadClientChan.
	// We can't in fact, since if we close them then client goroutines may panic.
	// The client goroutines won't block sending to them as they will check
	// ShutdownChan.

	s.WG.Wait()

	return nil
}

func (s *Server) shutdown() {
	log.Printf("Server shutdown initiated.")

	// Closing ShutdownChan indicates to other goroutines that we're shutting
	// down.
	close(s.ShutdownChan)

	err := s.Listener.Close()
	if err != nil {
		log.Printf("Problem closing TCP listener: %s", err)
	}

	// All clients need to be told. This also closes their write channels.
	for _, client := range s.Clients {
		client.quit("Server shutting down")
	}
}

// acceptConnections accepts TCP connections and tells the main server loop
// through a channel. It sets up separate goroutines for reading/writing to
// and from the client.
func (s *Server) acceptConnections(newClientChan chan<- *Client,
	messageServerChan chan<- ClientMessage, deadClientChan chan<- *Client) {
	defer s.WG.Done()

	id := uint64(0)

	for {
		if s.shuttingDown() {
			log.Printf("Connection accepter shutting down.")
			close(newClientChan)
			return
		}

		conn, err := s.Listener.Accept()
		if err != nil {
			log.Printf("Failed to accept connection: %s", err)
			continue
		}

		clientWriteChan := make(chan irc.Message, 100)

		client := &Client{
			Conn:      irc.NewConn(conn),
			WriteChan: clientWriteChan,
			ID:        id,
			Channels:  make(map[string]*Channel),
			Server:    s,
			Modes:     make(map[byte]struct{}),
		}

		// We're doing reads/writes in separate goroutines. No need for timeout.
		client.Conn.IOTimeoutDuration = 0

		// Handle rollover of uint64. Unlikely to happen (outside abuse) but.
		if id+1 == 0 {
			log.Fatalf("Unique ids rolled over!")
		}
		id++

		tcpAddr, err := net.ResolveTCPAddr("tcp", conn.RemoteAddr().String())
		// This shouldn't happen.
		if err != nil {
			log.Fatalf("Unable to resolve TCP address: %s", err)
		}

		client.IP = tcpAddr.IP

		s.WG.Add(1)
		go client.readLoop(messageServerChan, deadClientChan)
		s.WG.Add(1)
		go client.writeLoop(deadClientChan)

		newClientChan <- client
	}
}

func (s *Server) shuttingDown() bool {
	// No messages get sent to this channel, so if we receive a message on it,
	// then we know the channel was closed.
	select {
	case <-s.ShutdownChan:
		return true
	default:
		return false
	}
}

// Alarm sends a message to the server goroutine to wake it up.
// It sleeps and then repeats.
func (s *Server) alarm(toServer chan<- struct{}) {
	defer s.WG.Done()

	for {
		time.Sleep(s.WakeupTime)

		toServer <- struct{}{}

		if s.shuttingDown() {
			log.Printf("Alarm shutting down.")
			close(toServer)
			return
		}
	}
}

// checkAndPingClients looks at each connected client.
//
// If they've been idle a short time, we send them a PING (if they're
// registered).
//
// If they've been idle a long time, we kill their connection.
func (s *Server) checkAndPingClients() {
	now := time.Now()

	for _, client := range s.Clients {
		timeIdle := now.Sub(client.LastActivityTime)
		timeSincePing := now.Sub(client.LastPingTime)

		if client.Registered {
			// Was it active recently enough that we don't need to do anything?
			if timeIdle < s.PingTime {
				continue
			}

			// It's been idle a while.

			// Has it been idle long enough that we consider it dead?
			if timeIdle > s.DeadTime {
				client.quit(fmt.Sprintf("Ping timeout: %d seconds",
					int(timeIdle.Seconds())))
				continue
			}

			// Should we ping it? We might have pinged it recently.
			if timeSincePing < s.PingTime {
				continue
			}

			s.messageClient(client, "PING", []string{s.Config["server-name"]})
			client.LastPingTime = now
			continue
		}

		if timeIdle > s.DeadTime {
			client.quit("Idle too long.")
		}
	}
}

// Send an IRC message to a client. Appears to be from the server.
// This works by writing to a client's channel.
func (s *Server) messageClient(c *Client, command string, params []string) {
	// For numeric messages, we need to prepend the nick.
	// Use * for the nick in cases where the client doesn't have one yet.
	// This is what ircd-ratbox does. Maybe not RFC...
	isNumeric := true
	for _, c := range command {
		if c < 48 || c > 57 {
			isNumeric = false
		}
	}

	if isNumeric {
		nick := "*"
		if len(c.Nick) > 0 {
			nick = c.Nick
		}
		newParams := []string{nick}

		newParams = append(newParams, params...)
		params = newParams
	}

	c.WriteChan <- irc.Message{
		Prefix:  s.Config["server-name"],
		Command: command,
		Params:  params,
	}
}

// handleMessage takes action based on a client's IRC message.
func (s *Server) handleMessage(c *Client, m irc.Message) {
	// Record that client said something to us just now.
	c.LastActivityTime = time.Now()

	// Clients SHOULD NOT (section 2.3) send a prefix. I'm going to disallow it
	// completely for all commands.
	if m.Prefix != "" {
		s.messageClient(c, "ERROR", []string{"Do not send a prefix"})
		return
	}

	// Non-RFC command that appears to be widely supported. Just ignore it for
	// now.
	if m.Command == "CAP" {
		return
	}

	if m.Command == "NICK" {
		s.nickCommand(c, m)
		return
	}

	if m.Command == "USER" {
		s.userCommand(c, m)
		return
	}

	// Let's say *all* other commands require you to be registered.
	// This is likely stricter than RFC.
	if !c.Registered {
		// 451 ERR_NOTREGISTERED
		s.messageClient(c, "451", []string{fmt.Sprintf("You have not registered.")})
		return
	}

	if m.Command == "JOIN" {
		s.joinCommand(c, m)
		return
	}

	if m.Command == "PART" {
		s.partCommand(c, m)
		return
	}

	if m.Command == "PRIVMSG" {
		s.privmsgCommand(c, m)
		return
	}

	if m.Command == "LUSERS" {
		s.lusersCommand(c)
		return
	}

	if m.Command == "MOTD" {
		s.motdCommand(c)
		return
	}

	if m.Command == "QUIT" {
		s.quitCommand(c, m)
		return
	}

	if m.Command == "PONG" {
		// Not doing anything with this. Just accept it.
		return
	}

	if m.Command == "PING" {
		s.pingCommand(c, m)
		return
	}

	if m.Command == "DIE" {
		s.dieCommand(c, m)
		return
	}

	if m.Command == "WHOIS" {
		s.whoisCommand(c, m)
		return
	}

	if m.Command == "OPER" {
		s.operCommand(c, m)
		return
	}

	if m.Command == "MODE" {
		s.modeCommand(c, m)
		return
	}

	if m.Command == "WHO" {
		s.whoCommand(c, m)
		return
	}

	// Unknown command. We don't handle it yet anyway.

	// 421 ERR_UNKNOWNCOMMAND
	s.messageClient(c, "421", []string{m.Command, "Unknown command"})
}

func (s *Server) nickCommand(c *Client, m irc.Message) {
	// We should have one parameter: The nick they want.
	if len(m.Params) == 0 {
		// 431 ERR_NONICKNAMEGIVEN
		s.messageClient(c, "431", []string{"No nickname given"})
		return
	}

	// We could check if there is more than 1 parameter. But it doesn't seem
	// particularly problematic if there are. We ignore them. There's not a good
	// error to raise in RFC even if we did check.

	nick := m.Params[0]

	if !isValidNick(nick) {
		// 432 ERR_ERRONEUSNICKNAME
		s.messageClient(c, "432", []string{nick, "Erroneous nickname"})
		return
	}

	// Nick must be caselessly unique.
	nickCanon := canonicalizeNick(nick)

	_, exists := s.Nicks[nickCanon]
	if exists {
		// 433 ERR_NICKNAMEINUSE
		s.messageClient(c, "432", []string{nick, "Nickname is already in use"})
		return
	}

	// Flag the nick as taken by this client.
	s.Nicks[nickCanon] = c
	oldNick := c.Nick

	// The NICK command to happen both at connection registration time and
	// after. There are different rules.

	// Free the old nick (if there is one).
	// I do this in both registered and not states in case there are clients
	// misbehaving. I suppose we could not let them issue any more NICKs
	// beyond the first too if they are not registered.
	if len(oldNick) > 0 {
		delete(s.Nicks, canonicalizeNick(oldNick))
	}

	if c.Registered {
		// We need to inform other clients about the nick change.
		// Any that are in the same channel as this client.
		informedClients := map[uint64]struct{}{}
		for _, channel := range c.Channels {
			for _, member := range channel.Members {
				// Tell each client only once.
				_, exists := informedClients[member.ID]
				if exists {
					continue
				}

				// Message needs to come from the OLD nick.
				c.messageClient(member, "NICK", []string{nick})
				informedClients[member.ID] = struct{}{}
			}
		}

		// Reply to the client. We should have above, but if they were not on any
		// channels then we did not.
		_, exists := informedClients[c.ID]
		if !exists {
			c.messageClient(c, "NICK", []string{nick})
		}
	}

	// We don't reply during registration (we don't have enough info, no uhost
	// anyway).

	// Finally, make the update. Do this last as we need to ensure we act
	// as the old nick when crafting messages.
	c.Nick = nick
}

func (s *Server) userCommand(c *Client, m irc.Message) {
	// The USER command only occurs during connection registration.
	if c.Registered {
		// 462 ERR_ALREADYREGISTRED
		s.messageClient(c, "462",
			[]string{"Unauthorized command (already registered)"})
		return
	}

	// I'm going to require NICK before user. RFC RECOMMENDs this.
	if len(c.Nick) == 0 {
		// No good error code that I see.
		s.messageClient(c, "ERROR", []string{"Please send NICK first"})
		return
	}

	// 4 parameters: <user> <mode> <unused> <realname>
	if len(m.Params) != 4 {
		// 461 ERR_NEEDMOREPARAMS
		s.messageClient(c, "461", []string{m.Command, "Not enough parameters"})
		return
	}

	user := m.Params[0]

	if !isValidUser(user) {
		// There isn't an appropriate response in the RFC. ircd-ratbox sends an
		// ERROR message. Do that.
		s.messageClient(c, "ERROR", []string{"Invalid username"})
		return
	}
	c.User = user

	// We could do something with user mode here.

	// Validate realname.
	// Arbitrary. Length only.
	if len(m.Params[3]) > 64 {
		s.messageClient(c, "ERROR", []string{"Invalid realname"})
		return
	}
	c.RealName = m.Params[3]

	// This completes connection registration.

	c.Registered = true

	// RFC 2813 specifies messages to send upon registration.

	// 001 RPL_WELCOME
	s.messageClient(c, "001", []string{
		fmt.Sprintf("Welcome to the Internet Relay Network %s", c.nickUhost()),
	})

	// 002 RPL_YOURHOST
	s.messageClient(c, "002", []string{
		fmt.Sprintf("Your host is %s, running version %s", s.Config["server-name"],
			s.Config["version"]),
	})

	// 003 RPL_CREATED
	s.messageClient(c, "003", []string{
		fmt.Sprintf("This server was created %s", s.Config["created-date"]),
	})

	// 004 RPL_MYINFO
	// <servername> <version> <available user modes> <available channel modes>
	s.messageClient(c, "004", []string{
		// It seems ambiguous if these are to be separate parameters.
		s.Config["server-name"],
		s.Config["version"],
		"o",
		"n",
	})

	s.lusersCommand(c)

	s.motdCommand(c)
}

func (s *Server) joinCommand(c *Client, m irc.Message) {
	// Parameters: ( <channel> *( "," <channel> ) [ <key> *( "," <key> ) ] ) / "0"

	if len(m.Params) == 0 {
		// 461 ERR_NEEDMOREPARAMS
		s.messageClient(c, "461", []string{"JOIN", "Not enough parameters"})
		return
	}

	// JOIN 0 is a special case. Client leaves all channels.
	if len(m.Params) == 1 && m.Params[0] == "0" {
		for _, channel := range c.Channels {
			c.part(channel.Name, "")
		}
		return
	}

	// Again, we could check if there are too many parameters, but we just
	// ignore them.

	// NOTE: I choose to not support comma separated channels. RFC 2812
	//   allows multiple channels in a single command.

	channelName := canonicalizeChannel(m.Params[0])
	if !isValidChannel(channelName) {
		// 403 ERR_NOSUCHCHANNEL. Used to indicate channel name is invalid.
		s.messageClient(c, "403", []string{channelName, "Invalid channel name"})
		return
	}

	// TODO: Support keys.

	// Try to join the client to the channel.

	// Is the client in the channel already?
	if c.onChannel(&Channel{Name: channelName}) {
		// We could just ignore it too.
		s.messageClient(c, "ERROR", []string{"You are on that channel"})
		return
	}

	// Look up / create the channel
	channel, exists := s.Channels[channelName]
	if !exists {
		channel = &Channel{
			Name:    channelName,
			Members: make(map[uint64]*Client),
		}
		s.Channels[channelName] = channel
	}

	// Add the client to the channel.
	channel.Members[c.ID] = c
	c.Channels[channelName] = channel

	// Tell the client about the join. This is what RFC says to send:
	// Send JOIN, RPL_TOPIC, and RPL_NAMREPLY.

	// JOIN comes from the client, to the client.
	c.messageClient(c, "JOIN", []string{channel.Name})

	// It appears RPL_TOPIC is optional, at least ircd-ratbox does not send it.
	// Presumably if there is no topic.
	// TODO: Send topic when we have one.

	// RPL_NAMREPLY: This tells the client about who is in the channel
	// (including itself).
	// It ends with RPL_ENDOFNAMES.
	for _, member := range channel.Members {
		// 353 RPL_NAMREPLY
		s.messageClient(c, "353", []string{
			// = means public channel. TODO: When we have chan modes +s / +p this
			// needs to vary
			// TODO: We need to include @ / + for each nick opped/voiced.
			// Note we can have multiple nicks per RPL_NAMREPLY. TODO: Do that.
			"=", channel.Name, fmt.Sprintf(":%s", member.Nick),
		})
	}

	// 366 RPL_ENDOFNAMES
	s.messageClient(c, "366", []string{channel.Name, "End of NAMES list"})

	// Tell each member in the channel about the client.
	for _, member := range channel.Members {
		// Don't tell the client. We already did (above).
		if member.ID == c.ID {
			continue
		}

		// From the client to each member.
		c.messageClient(member, "JOIN", []string{channel.Name})
	}
}

func (s *Server) partCommand(c *Client, m irc.Message) {
	// Parameters: <channel> *( "," <channel> ) [ <Part Message> ]

	if len(m.Params) == 0 {
		// 461 ERR_NEEDMOREPARAMS
		s.messageClient(c, "461", []string{"PART", "Not enough parameters"})
		return
	}

	// Again, we don't raise error if there are too many parameters.

	partMessage := ""
	if len(m.Params) >= 2 {
		partMessage = m.Params[1]
	}

	c.part(m.Params[0], partMessage)
}

func (s *Server) privmsgCommand(c *Client, m irc.Message) {
	// Parameters: <msgtarget> <text to be sent>

	if len(m.Params) == 0 {
		// 411 ERR_NORECIPIENT
		s.messageClient(c, "411", []string{"No recipient given (PRIVMSG)"})
		return
	}

	if len(m.Params) == 1 {
		// 412 ERR_NOTEXTTOSEND
		s.messageClient(c, "412", []string{"No text to send"})
		return
	}

	// I don't check if there are too many parameters. They get ignored anyway.

	target := m.Params[0]

	msg := m.Params[1]

	// The message may be too long once we add the prefix/encode the message.
	// Strip any trailing characters until it's short enough.
	// TODO: Other messages can have this problem too (PART, QUIT, etc...)
	msgLen := len(":") + len(c.nickUhost()) + len(" PRIVMSG ") + len(target) +
		len(" ") + len(":") + len(msg) + len("\r\n")
	if msgLen > irc.MaxLineLength {
		trimCount := msgLen - irc.MaxLineLength
		msg = msg[:len(msg)-trimCount]
	}

	// I only support # channels right now.

	if target[0] == '#' {
		channelName := canonicalizeChannel(target)
		if !isValidChannel(channelName) {
			// 404 ERR_CANNOTSENDTOCHAN
			s.messageClient(c, "404", []string{channelName, "Cannot send to channel"})
			return
		}

		channel, exists := s.Channels[channelName]
		if !exists {
			// 403 ERR_NOSUCHCHANNEL
			s.messageClient(c, "403", []string{channelName, "No such channel"})
			return
		}

		// Are they on it?
		// TODO: Technically we should allow messaging if they aren't on it
		//   depending on the mode.
		if !c.onChannel(channel) {
			// 404 ERR_CANNOTSENDTOCHAN
			s.messageClient(c, "404", []string{channelName, "Cannot send to channel"})
			return
		}

		// Send to all members of the channel. Except the client itself it seems.
		for _, member := range channel.Members {
			if member.ID == c.ID {
				continue
			}

			// From the client to each member.
			c.messageClient(member, "PRIVMSG", []string{channel.Name, msg})
		}

		return
	}

	// We're messaging a nick directly.

	nickName := canonicalizeNick(target)
	if !isValidNick(nickName) {
		// 401 ERR_NOSUCHNICK
		s.messageClient(c, "401", []string{nickName, "No such nick/channel"})
		return
	}

	targetClient, exists := s.Nicks[nickName]
	if !exists {
		// 401 ERR_NOSUCHNICK
		s.messageClient(c, "401", []string{nickName, "No such nick/channel"})
		return
	}

	c.messageClient(targetClient, "PRIVMSG", []string{nickName, msg})
}

func (s *Server) lusersCommand(c *Client) {
	// We always send RPL_LUSERCLIENT and RPL_LUSERME.
	// The others only need be sent if the counts are non-zero.

	// 251 RPL_LUSERCLIENT
	s.messageClient(c, "251", []string{
		fmt.Sprintf("There are %d users and %d services on %d servers.",
			len(s.Nicks), 0, 0),
	})

	// 252 RPL_LUSEROP
	// TODO: When we have operators.

	// 253 RPL_LUSERUNKNOWN
	// Unregistered connections.
	numUnknown := len(s.Clients) - len(s.Nicks)
	if numUnknown > 0 {
		s.messageClient(c, "253", []string{
			fmt.Sprintf("%d", numUnknown),
			"unknown connection(s)",
		})
	}

	// 254 RPL_LUSERCHANNELS
	if len(s.Channels) > 0 {
		s.messageClient(c, "254", []string{
			fmt.Sprintf("%d", len(s.Channels)),
			"channels formed",
		})
	}

	// 255 RPL_LUSERME
	s.messageClient(c, "255", []string{
		fmt.Sprintf("I have %d clients and %d servers",
			len(s.Nicks), 0),
	})
}

func (s *Server) motdCommand(c *Client) {
	// 375 RPL_MOTDSTART
	s.messageClient(c, "375", []string{
		fmt.Sprintf("- %s Message of the day - ", s.Config["server-name"]),
	})

	// 372 RPL_MOTD
	s.messageClient(c, "372", []string{
		fmt.Sprintf("- %s", s.Config["motd"]),
	})

	// 376 RPL_ENDOFMOTD
	s.messageClient(c, "376", []string{"End of MOTD command"})
}

func (s *Server) quitCommand(c *Client, m irc.Message) {
	msg := "Quit:"
	if len(m.Params) > 0 {
		msg += " " + m.Params[0]
	}

	c.quit(msg)
}

func (s *Server) pingCommand(c *Client, m irc.Message) {
	// Parameters: <server> (I choose to not support forwarding)
	if len(m.Params) == 0 {
		// 409 ERR_NOORIGIN
		s.messageClient(c, "409", []string{"No origin specified"})
		return
	}

	server := m.Params[0]

	if server != s.Config["server-name"] {
		// 402 ERR_NOSUCHSERVER
		s.messageClient(c, "402", []string{server, "No such server"})
		return
	}

	s.messageClient(c, "PONG", []string{server})
}

func (s *Server) dieCommand(c *Client, m irc.Message) {
	// TODO: Operators only.

	// die is not an RFC command. I use it to shut down the server.
	s.shutdown()
}

func (s *Server) whoisCommand(c *Client, m irc.Message) {
	// Difference from RFC: I support only a single nickname (no mask), and no
	// server target.
	if len(m.Params) == 0 {
		// 431 ERR_NONICKNAMEGIVEN
		s.messageClient(c, "431", []string{"No nickname given"})
		return
	}

	nick := m.Params[0]
	nickCanonical := canonicalizeNick(nick)

	targetClient, exists := s.Nicks[nickCanonical]
	if !exists {
		// 401 ERR_NOSUCHNICK
		s.messageClient(c, "401", []string{nick, "No such nick/channel"})
		return
	}

	// 311 RPL_WHOISUSER
	s.messageClient(c, "311", []string{
		targetClient.Nick,
		targetClient.User,
		fmt.Sprintf("%s", targetClient.IP),
		"*",
		targetClient.RealName,
	})

	// 319 RPL_WHOISCHANNELS
	// I choose to not show any.

	// 312 RPL_WHOISSERVER
	s.messageClient(c, "312", []string{
		targetClient.Nick,
		s.Config["server-name"],
		s.Config["server-info"],
	})

	// 301 RPL_AWAY
	// TODO: AWAY not implemented yet.

	// 313 RPL_WHOISOPERATOR
	if targetClient.isOperator() {
		s.messageClient(c, "313", []string{
			targetClient.Nick,
			"is an IRC operator",
		})
	}

	// TODO: TLS information

	// 317 RPL_WHOISIDLE
	idleDuration := time.Now().Sub(targetClient.LastActivityTime)
	idleSeconds := int(idleDuration.Seconds())
	s.messageClient(c, "317", []string{
		targetClient.Nick,
		fmt.Sprintf("%d", idleSeconds),
		"seconds idle",
	})

	// 318 RPL_ENDOFWHOIS
	s.messageClient(c, "318", []string{
		targetClient.Nick,
		"End of WHOIS list",
	})
}

func (s *Server) operCommand(c *Client, m irc.Message) {
	// Parameters: <name> <password>
	if len(m.Params) < 2 {
		// 461 ERR_NEEDMOREPARAMS
		s.messageClient(c, "461", []string{"OPER", "Not enough parameters"})
		return
	}

	if c.isOperator() {
		s.messageClient(c, "ERROR", []string{"You are already an operator."})
		return
	}

	// TODO: Host matching

	// Check if they gave acceptable permissions.
	pass, exists := s.Opers[m.Params[0]]
	if !exists || pass != m.Params[1] {
		// 464 ERR_PASSWDMISMATCH
		s.messageClient(c, "464", []string{"Password incorrect"})
		return
	}

	// Give them oper status.
	c.Modes['o'] = struct{}{}

	c.messageClient(c, "MODE", []string{c.Nick, "+o"})

	// 381 RPL_YOUREOPER
	s.messageClient(c, "381", []string{"You are now an IRC operator"})
}

// MODE command applies either to nicknames or to channels.
func (s *Server) modeCommand(c *Client, m irc.Message) {
	// User mode:
	// Parameters: <nickname> *( ( "+" / "-" ) *( "i" / "w" / "o" / "O" / "r" ) )

	// Channel mode:
	// Parameters: <channel> *( ( "-" / "+" ) *<modes> *<modeparams> )

	if len(m.Params) < 1 {
		// 461 ERR_NEEDMOREPARAMS
		s.messageClient(c, "461", []string{"MODE", "Not enough parameters"})
		return
	}

	target := m.Params[0]

	// We can have blank mode. This will cause server to send current settings.
	modes := ""
	if len(m.Params) > 1 {
		modes = m.Params[1]
	}

	// Is it a nickname?
	targetClient, exists := s.Nicks[canonicalizeNick(target)]
	if exists {
		s.userModeCommand(c, targetClient, modes)
		return
	}

	// Is it a channel?
	targetChannel, exists := s.Channels[canonicalizeChannel(target)]
	if exists {
		s.channelModeCommand(c, targetChannel, modes)
		return
	}

	// Well... Not found. Send a channel not found. It seems the closest matching
	// extant error in RFC.
	// 403 ERR_NOSUCHCHANNEL
	s.messageClient(c, "403", []string{target, "No such channel"})
}

func (s *Server) userModeCommand(c, targetClient *Client, modes string) {
	// They can only change their own mode.
	if targetClient != c {
		// 502 ERR_USERSDONTMATCH
		s.messageClient(c, "502", []string{"Cannot change mode for other users"})
		return
	}

	// No modes given means we should send back their current mode.
	if len(modes) == 0 {
		modeReturn := "+"
		for k := range c.Modes {
			modeReturn += string(k)
		}

		// 221 RPL_UMODEIS
		s.messageClient(c, "221", []string{modeReturn})
		return
	}

	action := ' '
	for _, char := range modes {
		if char == '+' || char == '-' {
			action = char
			continue
		}

		if action == ' ' {
			// Malformed. No +/-.
			s.messageClient(c, "ERROR", []string{"Malformed MODE"})
			continue
		}

		// Only mode I support right now is 'o' (operator).
		// But some others I will ignore silently to avoid clients getting unknown
		// mode messages.
		if char == 'i' || char == 'w' || char == 's' {
			continue
		}

		if char != 'o' {
			// 501 ERR_UMODEUNKNOWNFLAG
			s.messageClient(c, "501", []string{"Unknown MODE flag"})
			continue
		}

		// Ignore it if they try to +o (operator) themselves. RFC says to do so.
		if action == '+' {
			continue
		}

		// This is -o. They have to be operator for there to be any effect.
		if !c.isOperator() {
			continue
		}

		delete(c.Modes, 'o')
		c.messageClient(c, "MODE", []string{"-o", c.Nick})
	}
}

func (s *Server) channelModeCommand(c *Client, channel *Channel,
	modes string) {
	if !c.onChannel(channel) {
		// 442 ERR_NOTONCHANNEL
		s.messageClient(c, "442", []string{channel.Name, "You're not on that channel"})
		return
	}

	// No modes? Send back the channel's modes.
	// Always send back +n. That's only I support right now.
	if len(modes) == 0 {
		// 324 RPL_CHANNELMODEIS
		s.messageClient(c, "324", []string{channel.Name, "+n"})
		return
	}

	// Listing bans. I don't support bans at this time, but say that there are
	// none.
	if modes == "b" || modes == "+b" {
		// 368 RPL_ENDOFBANLIST
		s.messageClient(c, "368", []string{channel.Name, "End of channel ban list"})
		return
	}

	// Since I don't have channel operators implemented, all attempts to alter
	// mode is an error.
	// 482 ERR_CHANOPRIVSNEEDED
	s.messageClient(c, "482", []string{channel.Name, "You're not channel operator"})
}

func (s *Server) whoCommand(c *Client, m irc.Message) {
	// Contrary to RFC 2812, I support only 'WHO #channel'.
	if len(m.Params) < 1 {
		// 461 ERR_NEEDMOREPARAMS
		s.messageClient(c, "461", []string{m.Command, "Not enough parameters"})
		return
	}

	channel, exists := s.Channels[canonicalizeChannel(m.Params[0])]
	if !exists {
		// 403 ERR_NOSUCHCHANNEL. Used to indicate channel name is invalid.
		c.Server.messageClient(c, "403", []string{m.Params[0], "Invalid channel name"})
		return
	}

	// Only works if they are on the channel.
	if !c.onChannel(channel) {
		// 442 ERR_NOTONCHANNEL
		s.messageClient(c, "442", []string{channel.Name, "You're not on that channel"})
		return
	}

	for _, member := range channel.Members {
		// 352 RPL_WHOREPLY
		// "<channel> <user> <host> <server> <nick>
		// ( "H" / "G" > ["*"] [ ( "@" / "+" ) ]
		// :<hopcount> <real name>"
		// NOTE: I'm not sure what H/G mean.
		// Hopcount seems unimportant also.
		mode := "H"
		if member.isOperator() {
			mode += "*"
		}
		s.messageClient(c, "352", []string{
			channel.Name, member.User, fmt.Sprintf("%s", member.IP),
			s.Config["server-name"], member.Nick,
			mode, "0 " + member.RealName,
		})
	}

	// 315 RPL_ENDOFWHO
	s.messageClient(c, "315", []string{channel.Name, "End of WHO list"})
}

// Send an IRC message to a client from another client.
// The server is the one sending it, but it appears from the client through use
// of the prefix.
//
// This works by writing to a client's channel.
func (c *Client) messageClient(to *Client, command string, params []string) {
	to.WriteChan <- irc.Message{
		Prefix:  c.nickUhost(),
		Command: command,
		Params:  params,
	}
}

func (c *Client) onChannel(channel *Channel) bool {
	_, exists := c.Channels[channel.Name]
	return exists
}

// readLoop endlessly reads from the client's TCP connection. It parses each
// IRC protocol message and passes it to the server through the server's
// channel.
func (c *Client) readLoop(messageServerChan chan<- ClientMessage,
	deadClientChan chan<- *Client) {
	defer c.Server.WG.Done()

	for {
		message, err := c.Conn.ReadMessage()
		if err != nil {
			log.Printf("Client %s: %s", c, err)
			// To not block forever if shutting down.
			select {
			case deadClientChan <- c:
			case <-c.Server.ShutdownChan:
			}
			return
		}

		// We want to tell the server about this message.
		// We also try to receive from the shutdown channel. This is so we will
		// not block forever when shutting down. The ShutdownChan closes when we
		// shutdown.
		select {
		case messageServerChan <- ClientMessage{Client: c, Message: message}:
		case <-c.Server.ShutdownChan:
			log.Printf("Client %s shutting down", c)
			return
		}
	}
}

// writeLoop endlessly reads from the client's channel, encodes each message,
// and writes it to the client's TCP connection.
func (c *Client) writeLoop(deadClientChan chan<- *Client) {
	defer c.Server.WG.Done()

	for message := range c.WriteChan {
		err := c.Conn.WriteMessage(message)
		if err != nil {
			log.Printf("Client %s: %s", c, err)
			// To not block forever if shutting down.
			select {
			case deadClientChan <- c:
			case <-c.Server.ShutdownChan:
			}
			break
		}
	}

	// Close the TCP connection. We do this here because we need to be sure we've
	// processed all messages to the client before closing the socket.
	err := c.Conn.Close()
	if err != nil {
		log.Printf("Client %s: Problem closing connection: %s", c, err)
	}

	log.Printf("Client %s write goroutine terminating.", c)
}

func (c *Client) String() string {
	return fmt.Sprintf("%d %s", c.ID, c.Conn.RemoteAddr())
}

func (c *Client) nickUhost() string {
	return fmt.Sprintf("%s!~%s@%s", c.Nick, c.User, c.IP)
}

// part tries to remove the client from the channel.
//
// We send a reply to the client. We also inform any other clients that need to
// know.
func (c *Client) part(channelName, message string) {
	// NOTE: Difference from RFC 2812: I only accept one channel at a time.
	channelName = canonicalizeChannel(channelName)

	if !isValidChannel(channelName) {
		// 403 ERR_NOSUCHCHANNEL. Used to indicate channel name is invalid.
		c.Server.messageClient(c, "403", []string{channelName, "Invalid channel name"})
		return
	}

	// Find the channel.
	channel, exists := c.Server.Channels[channelName]
	if !exists {
		// 403 ERR_NOSUCHCHANNEL. Used to indicate channel name is invalid.
		c.Server.messageClient(c, "403", []string{channelName, "No such channel"})
		return
	}

	// Are they on the channel?
	if !c.onChannel(channel) {
		// 403 ERR_NOSUCHCHANNEL. Used to indicate channel name is invalid.
		c.Server.messageClient(c, "403", []string{channelName, "You are not on that channel"})
		return
	}

	// Tell everyone (including the client) about the part.
	for _, member := range channel.Members {
		params := []string{channelName}

		// Add part message.
		if len(message) > 0 {
			params = append(params, message)
		}

		// From the client to each member.
		c.messageClient(member, "PART", params)
	}

	// Remove the client from the channel.
	delete(channel.Members, c.ID)
	delete(c.Channels, channel.Name)

	// If they are the last member, then drop the channel completely.
	if len(channel.Members) == 0 {
		delete(c.Server.Channels, channel.Name)
	}
}

func (c *Client) quit(msg string) {
	if c.Registered {
		// Tell all clients the client is in the channel with.
		// Also remove the client from each channel.
		toldClients := map[uint64]struct{}{}
		for _, channel := range c.Channels {
			for _, client := range channel.Members {
				_, exists := toldClients[client.ID]
				if exists {
					continue
				}

				c.messageClient(client, "QUIT", []string{msg})

				toldClients[client.ID] = struct{}{}
			}

			delete(channel.Members, c.ID)
			if len(channel.Members) == 0 {
				delete(c.Server.Channels, channel.Name)
			}
		}

		// Ensure we tell the client (e.g., if in no channels).
		_, exists := toldClients[c.ID]
		if !exists {
			c.messageClient(c, "QUIT", []string{msg})
		}

		delete(c.Server.Nicks, canonicalizeNick(c.Nick))
	} else {
		// May have set a nick.
		if len(c.Nick) > 0 {
			delete(c.Server.Nicks, canonicalizeNick(c.Nick))
		}
	}

	c.Server.messageClient(c, "ERROR", []string{msg})

	// Close their connection and channels.
	// Closing the channel leads to closing the TCP connection.
	close(c.WriteChan)

	delete(c.Server.Clients, c.ID)
}

func (c *Client) isOperator() bool {
	_, exists := c.Modes['o']
	return exists
}

// canonicalizeNick converts the given nick to its canonical representation
// (which must be unique).
//
// Note: We don't check validity or strip whitespace.
func canonicalizeNick(n string) string {
	return strings.ToLower(n)
}

// canonicalizeChannel converts the given channel to its canonical
// representation (which must be unique).
//
// Note: We don't check validity or strip whitespace.
func canonicalizeChannel(c string) string {
	return strings.ToLower(c)
}

// isValidNick checks if a nickname is valid.
func isValidNick(n string) bool {
	if len(n) == 0 || len(n) > maxNickLength {
		return false
	}

	// TODO: For now I accept only a-z, 0-9, or _. RFC is more lenient.
	for i, char := range n {
		if char >= 'a' && char <= 'z' {
			continue
		}

		if char >= '0' && char <= '9' {
			// No digits in first position.
			if i == 0 {
				return false
			}
			continue
		}

		if char == '_' {
			continue
		}

		return false
	}

	return true
}

// isValidUser checks if a user (USER command) is valid
func isValidUser(u string) bool {
	if len(u) == 0 || len(u) > maxNickLength {
		return false
	}

	// TODO: For now I accept only a-z or 0-9. RFC is more lenient.
	for _, char := range u {
		if char >= 'a' && char <= 'z' {
			continue
		}

		if char >= '0' && char <= '9' {
			continue
		}

		return false
	}

	return true
}

// isValidChannel checks a channel name for validity.
//
// You should canonicalize it before using this function.
func isValidChannel(c string) bool {
	if len(c) == 0 || len(c) > maxChannelLength {
		return false
	}

	// TODO: I accept only a-z or 0-9 as valid characters right now. RFC
	//   accepts more.
	for i, char := range c {
		if i == 0 {
			// TODO: I only allow # channels right now.
			if char == '#' {
				continue
			}
			return false
		}

		if char >= 'a' && char <= 'z' {
			continue
		}

		if char >= '0' && char <= '9' {
			continue
		}

		return false
	}

	return true
}
