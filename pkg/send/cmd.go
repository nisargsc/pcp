package send

import (
	"fmt"
	"os"

	"github.com/dennis-tra/pcp/pkg/words"

	"github.com/urfave/cli/v2"

	"github.com/dennis-tra/pcp/internal/log"
	"github.com/dennis-tra/pcp/pkg/config"
)

// Command holds the `send` subcommand configuration.
var Command = &cli.Command{
	Name:    "send",
	Usage:   "make the given file available to your peer",
	Aliases: []string{"s"},
	Action:  Action,
	Flags: []cli.Flag{
		&cli.IntFlag{
			Name:    "w",
			Aliases: []string{"word-count"},
			Usage:   "the number of random words to use (min 3)",
			EnvVars: []string{"PCP_WORD_COUNT"},
			Value:   4,
		},
	},
	ArgsUsage: `FILE`,
	Description: `
The send subcommand generates four random words based on the first
bytes of a newly generated peer identity. The first word and the
current time are used to generate an identifier that is broadcasted
in your local network via mDNS and provided through the distributed
hash table of the IPFS network.

After a peer attempts to connect it starts a password authen-
ticated key exchange (PAKE) with the remaining three words to
proof that the peer is in possession of the password. While this
is happening the tool still searches for other peers as the
currently connected one could fail the authentication procedure.

After the authentication was successful and the peer confirmed
the file transfer the transmission is started.
`,
}

// Action contains the logic for the `send` subcommand of the peercp program. It is
// mainly responsible for input parsing and service initialisation.
func Action(c *cli.Context) error {
	// Read config file and fill context with it.
	c, err := config.FillContext(c)
	if err != nil {
		return err
	}

	// Try to open the file to check if we have access and fail early.
	filepath := c.Args().First()
	if err = validateFile(filepath); err != nil {
		return err
	}

	log.Debugln("Validating given word count:", c.Int("w"))
	if c.Int("w") < 3 && !c.Bool("homebrew") {
		return fmt.Errorf("the number of words must not be less than 3")
	}

	// Generate the random words
	_, wrds, err := words.Random("english", c.Int("w"))
	if err != nil {
		return err
	}

	// If homebrew flag is set, overwrite generated words with well known list
	if c.Bool("homebrew") {
		wrds = words.HomebrewList()
	}

	// Start the libp2p node
	node, err := InitNode(c, filepath, wrds)
	if err != nil {
		return fmt.Errorf("failed to initialize node: %w", err)
	}

	// if mDNS is active, start advertising in the local network
	if isMDNSActive(c) {
		go node.StartAdvertisingMDNS()
	}

	// if DHT is active, start advertising in the wider area network
	if isDHTActive(c) {
		go node.StartAdvertisingDHT()
	}

	// Wait for the user to stop the tool or the transfer to finish.
	select {
	case <-c.Done():
		node.Shutdown()
		return nil
	case <-node.SigDone():
		return nil
	}
}

// validateFile tries to open the file at the given path to check
// if we have the correct permissions to read it. Further, it
// checks whether the filepath represents a directory. This is
// currently not supported.
func validateFile(filepath string) error {
	log.Debugln("Validating given file:", filepath)

	if filepath == "" {
		return fmt.Errorf("please specify the file you want to transfer")
	}

	f, err := os.Open(filepath)
	if err != nil {
		return err
	}
	defer f.Close()

	return nil
}

// isMDNSActive returns true if the user explicitly chose to only advertised via mDNS
// or if they didn't specify any preference.
func isMDNSActive(c *cli.Context) bool {
	return c.Bool("mdns") || c.Bool("dht") == c.Bool("mdns")
}

// isDHTActive returns true if the user explicitly chose to only advertised via the DHT
// or if they didn't specify any preference.
func isDHTActive(c *cli.Context) bool {
	return c.Bool("dht") || c.Bool("dht") == c.Bool("mdns")
}
