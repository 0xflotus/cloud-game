package room

import (
	"image"
	"io/ioutil"
	"log"
	"math/rand"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	emulator "github.com/giongto35/cloud-game/emulator"
	"github.com/giongto35/cloud-game/webrtc"
	storage "github.com/giongto35/cloud-game/worker/cloud-storage"
)

// Room is a game session. multi webRTC sessions can connect to a same game.
// A room stores all the channel for interaction between all webRTCs session and emulator
type Room struct {
	ID string

	// imageChannel is image stream received from director
	imageChannel <-chan *image.RGBA
	// audioChannel is audio stream received from director
	audioChannel <-chan float32
	// inputChannel is input stream from websocket send to room
	inputChannel chan<- int
	// State of room
	IsRunning bool
	// Done channel is to fire exit event when room is closed
	Done chan struct{}
	// List of peerconnections in the room
	rtcSessions []*webrtc.WebRTC
	// NOTE: Not in use, lock rtcSessions
	sessionsLock *sync.Mutex
	// Director is emulator
	director *emulator.Director
	// Cloud storage to store room state online
	onlineStorage *storage.Client
	// GameName
	gameName string
}

// NewRoom creates a new room
func NewRoom(roomID, gamePath, gameName string, onlineStorage *storage.Client) *Room {
	// if no roomID is given, generate it
	if roomID == "" {
		roomID = generateRoomID(gameName)
	}
	log.Println("Init new room", roomID, gameName)
	imageChannel := make(chan *image.RGBA, 30)
	audioChannel := make(chan float32, 30)
	inputChannel := make(chan int, 100)

	// create director
	director := emulator.NewDirector(roomID, imageChannel, audioChannel, inputChannel)

	room := &Room{
		ID: roomID,

		imageChannel:  imageChannel,
		audioChannel:  audioChannel,
		inputChannel:  inputChannel,
		rtcSessions:   []*webrtc.WebRTC{},
		sessionsLock:  &sync.Mutex{},
		director:      director,
		IsRunning:     true,
		onlineStorage: onlineStorage,

		Done: make(chan struct{}, 1),
	}

	go room.startVideo()
	go room.startAudio()

	// Check if room is on local storage, if not, pull from GCS to local storage
	go func(gamePath, gameName, roomID string) {
		// Check room is on local or fetch from server
		savepath := emulator.GetSavePath(roomID)
		log.Println("Check ", savepath, " on local : ", room.isGameOnLocal(savepath))
		if !room.isGameOnLocal(savepath) {
			// Fetch room from GCP to server
			log.Println("Load room from online storage", savepath)
			if err := room.saveOnlineRoomToLocal(roomID, savepath); err != nil {
				log.Printf("Warn: Room %s is not in online storage, error %s", roomID, err)
			}
		}

		if roomID != "" {
			gameName = getGameNameFromRoomID(roomID)
		}
		log.Printf("Room %s started. GameName: %s", roomID, gameName)
		path := gamePath + "/" + gameName
		director.Start(path)
		log.Printf("Room %s ended", roomID)

		start := time.Now()
		runtime.GC()
		log.Printf("GC takes %s\n", time.Since(start))
	}(gamePath, gameName, roomID)

	return room
}

func getGameNameFromRoomID(roomID string) string {
	parts := strings.Split(roomID, "-")
	if len(parts) <= 1 {
		return ""
	}
	return parts[1]
}

// generateRoomID generate a unique room ID containing 16 digits
func generateRoomID(gameName string) string {
	roomID := strconv.FormatInt(rand.Int63(), 16) + "-" + gameName
	log.Println("Generate Room ID", roomID)
	//roomID := uuid.Must(uuid.NewV4()).String()
	return roomID
}

func (r *Room) isGameOnLocal(savepath string) bool {
	_, err := os.Open(savepath)
	return err == nil
}

func (r *Room) AddConnectionToRoom(peerconnection *webrtc.WebRTC, playerIndex int) {
	peerconnection.AttachRoomID(r.ID)
	r.rtcSessions = append(r.rtcSessions, peerconnection)

	go r.startWebRTCSession(peerconnection, playerIndex)
}

func (r *Room) startWebRTCSession(peerconnection *webrtc.WebRTC, playerIndex int) {
	defer func() {
		if r := recover(); r != nil {
			log.Println("Warn: Recovered when sent to close inputChannel")
		}
	}()

	for input := range peerconnection.InputChannel {
		// NOTE: when room is no longer running. InputChannel needs to have extra event to go inside the loop
		if peerconnection.Done || !peerconnection.IsConnected() || !r.IsRunning {
			break
		}

		if peerconnection.IsConnected() {
			// the first 8 bits belong to player 1
			// the next 8 belongs to player 2 ...
			// We standardize and put it to inputChannel (16 bits)
			input = input << ((uint(playerIndex) - 1) * emulator.NumKeys)
			select {
			case r.inputChannel <- input:
			default:
			}
		}
	}

	log.Println("Peerconn done")
}

// RemoveSession removes a peerconnection from room and return true if there is no more room
func (r *Room) RemoveSession(w *webrtc.WebRTC) {
	log.Println("Cleaning session: ", w.ID)
	// TODO: get list of r.rtcSessions in lock
	for i, s := range r.rtcSessions {
		log.Println("found session: ", w.ID)
		if s.ID == w.ID {
			r.rtcSessions = append(r.rtcSessions[:i], r.rtcSessions[i+1:]...)
			log.Println("Removed session ", s.ID, " from room: ", r.ID)
			break
		}
	}
}

// TODO: Reuse for remove Session
func (r *Room) IsPCInRoom(w *webrtc.WebRTC) bool {
	if r == nil {
		return false
	}
	for _, s := range r.rtcSessions {
		if s.ID == w.ID {
			return true
		}
	}
	return false
}

func (r *Room) Close() {
	if !r.IsRunning {
		return
	}

	r.IsRunning = false
	log.Println("Closing room", r.ID)
	log.Println("Closing director of room ", r.ID)
	close(r.director.Done)
	log.Println("Closing input of room ", r.ID)
	close(r.inputChannel)
	close(r.Done)
	// Close here is a bit wrong because this read channel
	// Just dont close it, let it be gc
	//close(r.imageChannel)
	//close(r.audioChannel)
}

func (r *Room) SaveGame() error {
	onlineSaveFunc := func() error {
		// Try to save the game to gCloud
		if err := r.onlineStorage.SaveFile(r.ID, r.director.GetHashPath()); err != nil {
			return err
		}

		return nil
	}

	// TODO: Move to game view
	if err := r.director.SaveGame(onlineSaveFunc); err != nil {
		return err
	}

	return nil
}

// saveOnlineRoomToLocal save online room to local
func (r *Room) saveOnlineRoomToLocal(roomID string, savepath string) error {
	log.Println("Check if game is on cloud storage")
	// If the game is not on local server
	// Try to load from gcloud
	data, err := r.onlineStorage.LoadFile(roomID)
	if err != nil {
		return err
	}
	// Save the data fetched from gcloud to local server
	ioutil.WriteFile(savepath, data, 0644)

	return nil
}

func (r *Room) LoadGame() error {
	err := r.director.LoadGame()

	return err
}

func (r *Room) EmptySessions() bool {
	return len(r.rtcSessions) == 0
}

func (r *Room) IsRunningSessions() bool {
	// If there is running session
	for _, s := range r.rtcSessions {
		if s.IsConnected() {
			return true
		}
	}

	return false
}
