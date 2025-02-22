// vim: tabstop=2 shiftwidth=2

package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strings"
	"time"

	"github.com/Masterminds/log-go"
	"github.com/crooks/yamn/crandom"
	"github.com/crooks/yamn/idlog"
	"github.com/crooks/yamn/keymgr"
	"github.com/crooks/yamn/quickmail"
	//"github.com/codahale/blake2"
)

// Start the server process.  If run with --daemon, this will loop forever.
func loopServer() (err error) {
	// Initialize the Public Keyring
	Pubring = keymgr.NewPubring(
		cfg.Files.Pubring,
		cfg.Files.Mlist2,
	)
	// Fetch keyring and stats URLs
	timedURLFetch(cfg.Urls.Pubring, cfg.Files.Pubring)
	timedURLFetch(cfg.Urls.Mlist2, cfg.Files.Mlist2)
	// Initialize the Secret Keyring
	secret := keymgr.NewSecring(cfg.Files.Secring, cfg.Files.Pubkey)
	Pubring.ImportPubring()
	secret.ImportSecring()
	// Tell the secret keyring some basic info about this remailer
	secret.SetName(cfg.Remailer.Name)
	secret.SetAddress(cfg.Remailer.Address)
	secret.SetExit(cfg.Remailer.Exit)
	secret.SetValidity(cfg.Remailer.Keylife, cfg.Remailer.Keygrace)
	secret.SetVersion(version)
	// Create some dirs if they don't already exist
	createDirs()

	// Open the IDlog
	log.Tracef("Opening ID Log: %s", cfg.Files.IDlog)
	// NewInstance takes the filename and entry validity in days
	IDDb = idlog.NewIDLog(cfg.Files.IDlog, cfg.Remailer.IDexp)
	defer IDDb.Close()
	// Open the chunk DB
	log.Tracef("Opening the Chunk DB: %s", cfg.Files.ChunkDB)
	ChunkDb = OpenChunk(cfg.Files.ChunkDB)
	ChunkDb.SetExpire(cfg.Remailer.ChunkExpire)

	// Expire old entries in the ID Log
	idLogExpire()
	// Clean the chunk DB
	chunkClean()
	// Complain about poor configs
	nagOperator()
	// Run a key purge
	if purgeSecring(secret) == 0 {
		// If there are zero active keys, generate a new one.
		generateKeypair(secret)
	} else {
		/*
			If the operator changes his configuration, (such as
			upgrading to a new version or switching from exit to
			middleman), the published key will not match the
			configuration.  This element of code writes a new
			key.txt file with current settings.  This only needs to
			be done if we haven't generated a new key.
		*/
		refreshPubkey(secret)
	}

	log.Infof("Secret keyring contains %d keys", secret.Count())

	// Define triggers for timed events
	daily := time.Now()
	hourly := time.Now()
	dayOfMonth := time.Now().Day()
	oneDay := time.Duration(dayLength) * time.Second

	// Determine if this is a single run or the start of a Daemon
	runAsDaemon := cfg.Remailer.Daemon || flag.Daemon

	// Actually start the server loop
	if runAsDaemon {
		log.Infof("Starting YAMN server: %s", cfg.Remailer.Name)
		log.Infof("Detaching Pool processing")
		go serverPoolOutboundSend()
	} else {
		log.Infof("Performing routine remailer functions for: %s",
			cfg.Remailer.Name)
	}
	for {
		// Panic if the pooldir doesn't exist
		assertIsPath(cfg.Files.Pooldir)
		// Process the inbound Pool
		processInpool("i", secret)
		// Process the Maildir
		processMail(secret)

		// Midnight events
		if time.Now().Day() != dayOfMonth {
			log.Info("Performing midnight events")
			// Remove expired keys from memory and rewrite a
			// secring file without expired keys.
			if purgeSecring(secret) == 0 {
				generateKeypair(secret)
			}
			// Expire entries in the ID Log
			idLogExpire()
			// Expire entries in the chunker
			chunkClean()
			// Report daily throughput and reset to zeros
			stats.report()
			stats.reset()
			// Reset dayOfMonth to today
			dayOfMonth = time.Now().Day()
		}
		// Daily events
		if time.Since(daily) > oneDay {
			log.Info("Performing daily events")
			// Complain about poor configs
			nagOperator()
			// Reset today so we don't do these tasks for the next
			// 24 hours.
			daily = time.Now()
		}
		// Hourly events
		if time.Since(hourly) > time.Hour {
			log.Trace("Performing hourly events")
			/*
				The following two conditions try to import new
				pubring and mlist2 URLs.  If they fail, a
				warning is logged but no further action is
				taken.  It's better to have old keys/stats than
				none.
			*/
			// Retrieve Mlist2 and Pubring URLs
			if cfg.Urls.Fetch {
				timedURLFetch(
					cfg.Urls.Pubring,
					cfg.Files.Pubring,
				)
				timedURLFetch(
					cfg.Urls.Mlist2,
					cfg.Files.Mlist2,
				)
			}
			// Test to see if the pubring.mix file has been updated
			if Pubring.KeyRefresh() {
				log.Tracef(
					"Reimporting Public Keyring: %s",
					cfg.Files.Pubring,
				)
				Pubring.ImportPubring()
			}
			// Report throughput
			stats.report()
			hourly = time.Now()
		}

		// Break out of the loop if we're not running as a daemon
		if !runAsDaemon {
			break
		}

		// And rest a while
		time.Sleep(60 * time.Second)
	} // End of server loop
	return
}

// refreshPubkey updates an existing Public key file
func refreshPubkey(secret *keymgr.Secring) {
	tmpKey := cfg.Files.Pubkey + ".tmp"
	keyidstr := secret.WriteMyKey(tmpKey)
	log.Infof("Advertising keyid: %s", keyidstr)
	log.Tracef("Writing current public key to %s", tmpKey)
	// Overwrite the published key with the refreshed version
	log.Tracef("Renaming %s to %s", tmpKey, cfg.Files.Pubkey)
	err := os.Rename(tmpKey, cfg.Files.Pubkey)
	if err != nil {
		log.Warn(err)
	}
}

// purgeSecring deletes old keys and counts active ones.  If no active keys
// are found, it triggers a generation.
func purgeSecring(secret *keymgr.Secring) (active int) {
	active, expiring, expired, purged := secret.Purge()
	log.Infof(
		"Key purge complete. Active=%d, Expiring=%d, Expired=%d, "+
			"Purged=%d",
		active,
		expiring,
		expired,
		purged,
	)
	return
}

// generateKeypair creates a new keypair and publishes it
func generateKeypair(secret *keymgr.Secring) {
	log.Info("Generating and advertising a new key pair")
	pub, sec := eccGenerate()
	keyidstr := secret.Insert(pub, sec)
	log.Infof("Generated new keypair with keyid: %s", keyidstr)
	log.Info("Writing new Public Key to disc")
	secret.WritePublic(pub, keyidstr)
	log.Info("Inserting Secret Key into Secring")
	secret.WriteSecret(keyidstr)
}

// idLogExpire deletes old entries in the ID Log
func idLogExpire() {
	count, deleted := IDDb.Expire()
	log.Infof("ID Log: Expired=%d, Contains=%d", deleted, count)
}

// chunkClean expires entries from the chunk DB and deletes any stranded files
func chunkClean() {
	cret, cexp := ChunkDb.Expire()
	if cexp > 0 {
		log.Infof(
			"Chunk expiry complete. Retained=%d, Expired=%d\n",
			cret,
			cexp,
		)
	}
	fret, fdel := ChunkDb.Housekeep()
	if fdel > 0 {
		log.Infof(
			"Stranded chunk deletion: Retained=%d, Deleted=%d",
			fret,
			fdel,
		)
	}
}

// nagOperator prompts a remailer operator about poor practices.
func nagOperator() {
	// Complain about excessively small loop values.
	if cfg.Pool.Loop < 60 {
		log.Warnf(
			"Loop time of %d is excessively low. Will loop "+
				"every 60 seconds. A higher setting is recommended.",
			cfg.Pool.Loop,
		)
	}
	// Complain about high pool rates.
	if cfg.Pool.Rate > 90 && !flag.Send {
		log.Warnf(
			"Your pool rate of %d is excessively high. Unless "+
				"testing, a lower setting is recommended.",
			cfg.Pool.Rate,
		)
	}
	// Complain about running a remailer with flag_send
	if flag.Send && flag.Remailer {
		log.Warnf(
			"Your remailer will flush the outbound pool every "+
				"%d seconds. Unless you're testing, this is "+
				"probably not what you want.",
			cfg.Pool.Loop,
		)
	}
}

func createDirs() {
	var err error
	err = os.MkdirAll(cfg.Files.IDlog, 0700)
	if err != nil {
		log.Errorf(
			"Failed to create %s. %s",
			cfg.Files.IDlog,
			err,
		)
		os.Exit(1)
	}
	err = os.MkdirAll(cfg.Files.Pooldir, 0700)
	if err != nil {
		log.Errorf(
			"Failed to create %s. %s",
			cfg.Files.Pooldir,
			err,
		)
		os.Exit(1)
	}
	err = os.MkdirAll(cfg.Files.ChunkDB, 0700)
	if err != nil {
		log.Errorf(
			"Failed to create %s. %s",
			cfg.Files.ChunkDB,
			err,
		)
		os.Exit(1)
	}
	err = os.MkdirAll(cfg.Files.Maildir, 0700)
	if err != nil {
		log.Errorf(
			"Failed to create %s. %s",
			cfg.Files.Maildir,
			err,
		)
		os.Exit(1)
	}
	mdirnew := path.Join(cfg.Files.Maildir, "new")
	err = os.MkdirAll(mdirnew, 0700)
	if err != nil {
		log.Errorf("Failed to create %s. %s", mdirnew, err)
		os.Exit(1)
	}
	mdircur := path.Join(cfg.Files.Maildir, "cur")
	err = os.MkdirAll(mdircur, 0700)
	if err != nil {
		log.Errorf("Failed to create %s. %s", mdircur, err)
		os.Exit(1)
	}
	mdirtmp := path.Join(cfg.Files.Maildir, "tmp")
	err = os.MkdirAll(mdirtmp, 0700)
	if err != nil {
		log.Errorf("Failed to create %s. %s", mdirtmp, err)
		os.Exit(1)
	}
}

// decodeMsg is the actual YAMN message decoder.  It's output is always a
// pooled file, either in the Inbound or Outbound queue.
func decodeMsg(rawMsg []byte, secret *keymgr.Secring) (err error) {

	// At this point, rawMsg should always be messageBytes in length
	err = lenCheck(len(rawMsg), messageBytes)
	if err != nil {
		log.Error(err)
		return
	}

	d := newDecMessage(rawMsg)
	// Extract the top header
	header := newDecodeHeader(d.getHeader())
	recipientKeyID := header.getRecipientKeyID()
	recipientSK, err := secret.GetSK(recipientKeyID)
	if err != nil {
		log.Warnf("Failed to ascertain Recipient SK: %s", err)
		return
	}
	header.setRecipientSK(recipientSK)

	slotDataBytes, packetVersion, err := header.decode()
	if err != nil {
		log.Warnf("Header decode failed: %s", err)
		return
	}
	switch packetVersion {
	case 2:
		err = decodeV2(d, slotDataBytes)
	default:
		err = fmt.Errorf("cannot decode packet version %d", packetVersion)
	}
	return
}

func decodeV2(d *decMessage, slotDataBytes []byte) (err error) {
	// Convert the raw Slot Data Bytes to meaningful slotData.
	slotData := decodeSlotData(slotDataBytes)
	// Test uniqueness of packet ID
	if !IDDb.Unique(slotData.getPacketID()) {
		log.Trace("Discarding duplicate message (packet ID collision)")
		return
	}
	if !d.testAntiTag(slotData.getTagHash()) {
		log.Warn("Anti-tag digest mismatch")
		return
	}
	if slotData.ageTimestamp() > cfg.Remailer.MaxAge {
		log.Warnf(
			"Max packet age in days exceeded. Age=%d, Max=%d",
			slotData.ageTimestamp(),
			cfg.Remailer.MaxAge,
		)
		return
	}
	if slotData.ageTimestamp() < 0 {
		log.Warn("Packet timestamp is in the future. Rejecting")
		return
	}
	if slotData.getPacketType() == 0 {
		d.shiftHeaders()
		// Decode Intermediate
		inter := decodeIntermediate(slotData.packetInfo)
		d.decryptAll(slotData.aesKey, inter.aesIV12)
		/*
			The following conditional tests if we are the next hop
			in addition to being the current hop.  If we are, then
			it's better to store the message in the inbound pool.
			This prevents it being emailed back to us.
		*/
		if inter.getNextHop() == cfg.Remailer.Address {
			log.Info(
				"Message loops back to us. ",
				"Storing in pool instead of sending it.")
			outfileName := randPoolFilename("i")
			err = ioutil.WriteFile(
				outfileName,
				d.getPayload(),
				0600,
			)
			if err != nil {
				log.Warnf("Failed to write to pool: %s", err)
				return
			}
			stats.outLoop++
		} else {
			writeMessageToPool(inter.getNextHop(), d.getPayload())
			stats.outYamn++
			// Decide if we want to inject a dummy
			if !flag.NoDummy && crandom.Dice() < 55 {
				dummy()
				stats.outDummy++
			}
		} // End of local or remote delivery
	} else if slotData.getPacketType() == 1 {
		// Decode Exit
		final := decodeFinal(slotData.packetInfo)
		if final.getDeliveryMethod() == 255 {
			log.Trace("Discarding dummy message")
			stats.inDummy++
			return
		}
		// Decrypt the payload body
		// This could be done under Delivery Method 0 but, future
		// delivery methods (other than dummies) will require a
		// decrypted body.
		plain := d.decryptBody(
			slotData.getAesKey(),
			final.getAesIV(),
			final.getBodyBytes(),
		)
		// Test delivery methods
		switch final.getDeliveryMethod() {
		case 0:
			stats.inYamn++
			if !cfg.Remailer.Exit {
				if final.numChunks == 1 {
					// Need to randhop as we're not an exit
					// remailer
					randhop(plain)
				} else {
					log.Warn(
						"Randhopping doesn't support " +
							"multi-chunk messages. As " +
							"per Mixmaster, this " +
							"message will be dropped.",
					)
				}
				return
			}
			smtpMethod(plain, final)
		default:
			log.Warnf(
				"Unsupported Delivery Method: %d",
				final.getDeliveryMethod(),
			)
			return
		}
	} else {
		log.Warnf(
			"Unknown Packet Type: %d",
			slotData.getPacketType(),
		)
		return
	}
	return
}

// smtpMethod is concerned with final-hop processing.
func smtpMethod(plain []byte, final *slotFinal) {
	var err error
	if final.getNumChunks() == 1 {
		// If this is a single chunk message, pool it and get out.
		writePlainToPool(plain, "m")
		stats.outPlain++
		return
	}
	// We're an exit and this is a multi-chunk message
	chunkFilename := writePlainToPool(plain, "p")
	log.Tracef(
		"Pooled partial chunk. MsgID=%x, Num=%d, "+
			"Parts=%d, Filename=%s",
		final.getMessageID(),
		final.getChunkNum(),
		final.getNumChunks(),
		chunkFilename,
	)
	// Fetch the chunks info from the DB for the given message ID
	chunks := ChunkDb.Get(final.getMessageID(), final.getNumChunks())
	// This saves losts of -1's as slices start at 0 and chunks at 1
	cslot := final.getChunkNum() - 1
	// Test that the slot for this chunk is empty
	if chunks[cslot] != "" {
		log.Warnf(
			"Duplicate chunk %d in MsgID: %x",
			final.chunkNum,
			final.messageID,
		)
	}
	// Insert the new chunk into the slice
	chunks[cslot] = chunkFilename
	log.Tracef(
		"Chunk state: %s",
		strings.Join(chunks, ","),
	)
	// Test if all chunk slots are populated
	if IsPopulated(chunks) {
		newPoolFile := randPoolFilename("m")
		log.Tracef(
			"Assembling chunked message into %s",
			newPoolFile,
		)
		err = ChunkDb.Assemble(newPoolFile, chunks)
		if err != nil {
			log.Warnf("Chunk assembly failed: %s", err)
			// Don't return here or the bad chunk will remain in
			// the DB.
		}
		// Now the message is assembled into the Pool, the DB record
		// can be deleted
		ChunkDb.Delete(final.getMessageID())
		stats.outPlain++
	} else {
		// Write the updated chunk status to
		// the DB
		ChunkDb.Insert(final.getMessageID(), chunks)
	}
}

// randhop is a simplified client function that does single-hop encodings
func randhop(plainMsg []byte) {
	var err error
	if len(plainMsg) == 0 {
		log.Info("Zero-byte message during randhop, ignoring it.")
		return
	}
	// Make a single hop chain with a random node
	inChain := []string{"*"}
	final := newSlotFinal()
	var chain []string
	chain, err = makeChain(inChain)
	if err != nil {
		log.Warn(err)
		return
	}
	sendTo := chain[0]
	if len(chain) != 1 {
		err = fmt.Errorf("randhop chain must be single hop.  Got=%d", len(chain))
		panic(err)
	}
	log.Tracef("Performing a random hop to Exit Remailer: %s.", chain[0])
	yamnMsg := encodeMsg(plainMsg, chain, *final)
	writeMessageToPool(sendTo, yamnMsg)
	stats.outRandhop++
}

// remailerFoo responds to requests for remailer-* info
func remailerFoo(subject, sender string) (err error) {
	m := quickmail.NewMessage()
	m.Set("From", cfg.Remailer.Address)
	m.Set("To", sender)
	if strings.HasPrefix(subject, "remailer-key") {
		// remailer-key
		log.Tracef("remailer-key request from %s", sender)
		m.Set("Subject", fmt.Sprintf("Remailer key for %s", cfg.Remailer.Name))
		m.Filename = cfg.Files.Pubkey
		m.Prefix = "Here is the Mixmaster key:\n\n=-=-=-=-=-=-=-=-=-=-=-="
	} else if strings.HasPrefix(subject, "remailer-conf") {
		// remailer-conf
		log.Tracef("remailer-conf request from %s", sender)
		m.Set(
			"Subject",
			fmt.Sprintf("Capabilities of the %s remailer", cfg.Remailer.Name))
		m.Text(fmt.Sprintf("Remailer-Type: Mixmaster %s\n", version))
		m.Text("Supported Formats:\n   Mixmaster\n")
		m.Text(fmt.Sprintf("Pool size: %d\n", cfg.Pool.Size))
		m.Text(fmt.Sprintf("Maximum message size: %d kB\n", cfg.Remailer.MaxSize))
		m.Text("The following header lines will be filtered:\n")
		m.Text(
			fmt.Sprintf("\n$remailer{\"%s\"} = \"<%s>",
				cfg.Remailer.Name, cfg.Remailer.Address))
		if !cfg.Remailer.Exit {
			m.Text(" middle")
		}
		packetVersions := []string{"v2"}
		for _, v := range packetVersions {
			m.Text(fmt.Sprintf(" %s", v))
		}
		m.Text("\";\n")
		m.Text("\nSUPPORTED MIXMASTER (TYPE II) REMAILERS")
		var pubList []string
		pubList, err := keymgr.Headers(cfg.Files.Pubring)
		if err != nil {
			log.Infof("Could not read %s", cfg.Files.Pubring)
		} else {
			m.List(pubList)
		}
	} else if strings.HasPrefix(subject, "remailer-adminkey") {
		// remailer-adminkey
		log.Tracef("remailer-adminkey request from %s", sender)
		m.Set(
			"Subject",
			fmt.Sprintf("Admin key for the %s remailer", cfg.Remailer.Name))
		m.Filename = cfg.Files.Adminkey
	} else if strings.HasPrefix(subject, "remailer-help") {
		// remailer-help
		log.Tracef("remailer-help request from %s", sender)
		m.Set(
			"Subject",
			fmt.Sprintf("Your help request for the %s Anonymous Remailer",
				cfg.Remailer.Name))
		m.Filename = cfg.Files.Help
	} else {
		if len(subject) > 20 {
			// Truncate long subject headers before logging them
			subject = subject[:20]
		}
		err = fmt.Errorf("ignoring request for %s", subject)
		return
	}
	var msg []byte
	msg, err = m.Compile()
	if err != nil {
		log.Infof("Unable to send %s", subject)
		return
	}
	err = mailBytes(msg, []string{sender})
	if err != nil {
		log.Warnf("Failed to send %s to %s", subject, sender)
		return
	}
	return
}
