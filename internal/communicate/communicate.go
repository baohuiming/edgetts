package communicate

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"github.com/lib-x/edgetts/internal/businessConsts"
	"github.com/lib-x/edgetts/internal/communicateOption"
	"github.com/lib-x/edgetts/internal/validate"
	"golang.org/x/net/proxy"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	ssmlHeaderTemplate = "X-RequestId:%s\r\nContent-Type:application/ssml+xml\r\nX-Timestamp:%sZ\r\nPath:ssml\r\n\r\n"
	ssmlTemplate       = "<speak version='1.0' xmlns='http://www.w3.org/2001/10/synthesis' xml:lang='en-US'><voice name='%s'><prosody pitch='+0Hz' rate='%s' volume='%s'>%s</prosody></voice></speak>"
)

var (
	headerOnce        = &sync.Once{}
	escapeReplacer    = strings.NewReplacer(">", "&gt;", "<", "&lt;")
	communicateHeader http.Header
)

func init() {
	headerOnce.Do(func() {
		communicateHeader = makeHeaders()
	})
}

type Communicate struct {
	text                string
	voice               string
	voiceLanguageRegion string
	rate                string
	volume              string

	httpProxy        string
	socket5Proxy     string
	socket5ProxyUser string
	socket5ProxyPass string
	op               chan map[string]interface{}

	AudioDataIndex int
}

type textEntry struct {
	Text         string `json:"text"`
	Length       int64  `json:"Length"`
	BoundaryType string `json:"BoundaryType"`
}
type dataEntry struct {
	Offset   int       `json:"Offset"`
	Duration int       `json:"Duration"`
	Text     textEntry `json:"text"`
}
type metaDataEntry struct {
	Type string    `json:"Type"`
	Data dataEntry `json:"Data"`
}
type audioData struct {
	Data  []byte
	Index int
}

type unknownResponse struct {
	Message string
}

type unexpectedResponse struct {
	Message string
}

type noAudioReceived struct {
	Message string
}

type webSocketError struct {
	Message string
}

func NewCommunicate(text string, options ...communicateOption.Option) (*Communicate, error) {
	opts := &communicateOption.CommunicateOption{}
	for _, optFn := range options {
		optFn(opts)
	}
	opts.ApplyDefaultOption()

	if err := validate.WithCommunicateOption(opts); err != nil {
		return nil, err
	}
	return &Communicate{
		text:                text,
		voice:               opts.Voice,
		voiceLanguageRegion: opts.VoiceLangRegion,
		rate:                opts.Rate,
		volume:              opts.Volume,
		httpProxy:           opts.HttpProxy,
		socket5Proxy:        opts.Socket5Proxy,
		socket5ProxyUser:    opts.Socket5ProxyUser,
		socket5ProxyPass:    opts.Socket5ProxyPass,
	}, nil
}

// WriteStreamTo  write audio stream to io.WriteCloser
func (c *Communicate) WriteStreamTo(rc io.Writer) error {
	op, err := c.stream()
	if err != nil {
		return err
	}
	audioBinaryData := make([][][]byte, c.AudioDataIndex)
	for i := range op {
		if _, ok := i["end"]; ok {
			if len(audioBinaryData) == c.AudioDataIndex {
				break
			}
		}
		if t, ok := i["type"]; ok && t == "audio" {
			data := i["data"].(audioData)
			audioBinaryData[data.Index] = append(audioBinaryData[data.Index], data.Data)
		}
		if e, ok := i["error"]; ok {
			fmt.Printf("has error err: %v\n", e)
		}
	}

	for _, dataSlice := range audioBinaryData {
		for _, data := range dataSlice {
			rc.Write(data)
		}
	}
	return nil
}

func (c *Communicate) CloseOutput() {
	close(c.op)
}

func makeHeaders() http.Header {
	header := make(http.Header)
	header.Set("Pragma", "no-cache")
	header.Set("Cache-Control", "no-cache")
	header.Set("Origin", "chrome-extension://jdiccldimpdaibmpdkjnbmckianbfold")
	header.Set("Accept-Encoding", "gzip, deflate, br")
	header.Set("Accept-Language", "en-US,en;q=0.9")
	header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.77 Safari/537.36 Edg/91.0.864.41")
	return header
}

func (c *Communicate) stream() (<-chan map[string]interface{}, error) {
	texts := splitTextByByteLength(
		escape(removeIncompatibleCharacters(c.text)),
		calculateMaxMessageSize(c.voice, c.rate, c.volume),
	)
	c.AudioDataIndex = len(texts)

	finalUtterance := make(map[int]int)
	prevIdx := -1
	shiftTime := -1

	output := make(chan map[string]interface{})

	for idx, text := range texts {
		wsURL := businessConsts.EdgeWssEndpoint + "&ConnectionId=" + generateConnectID()
		dialer := websocket.Dialer{}
		setupWebSocketProxy(&dialer, c)

		conn, _, err := dialer.Dial(wsURL, communicateHeader)
		if err != nil {
			return nil, err
		}

		// Each message needs to have the proper date.
		date := currentTimeInMST()

		// Prepare the request to be sent to the service.
		//
		// Note sentenceBoundaryEnabled and wordBoundaryEnabled are actually supposed
		// to be booleans, but Edge Browser seems to send them as strings.
		//
		// This is a bug in Edge as Azure Cognitive Services actually sends them as
		// bool and not string. For now I will send them as bool unless it causes
		// any problems.
		//
		// Also pay close attention to double { } in request (escape for f-string).
		err = conn.WriteMessage(websocket.TextMessage, []byte(
			"X-Timestamp:"+date+"\r\n"+
				"Content-Type:application/json; charset=utf-8\r\n"+
				"Path:speech.config\r\n\r\n"+
				`{"context":{"synthesis":{"audio":{"metadataoptions":{"sentenceBoundaryEnabled":false,"wordBoundaryEnabled":true},"outputFormat":"audio-24khz-48kbitrate-mono-mp3"}}}}`+"\r\n",
		))
		if err != nil {
			conn.Close()
			return nil, err
		}

		err = conn.WriteMessage(websocket.TextMessage, []byte(
			ssmlHeadersAppendExtraData(
				generateConnectID(),
				date,
				makeSsml(string(text), c.voice, c.rate, c.volume),
			),
		))
		if err != nil {
			conn.Close()
			return nil, err
		}

		go func(conn *websocket.Conn, idx int) {
			// download indicates whether we should be expecting audio data,
			// this is so what we avoid getting binary data from the websocket
			// and falsely thinking it's audio data.
			downloadAudio := false

			// audioWasReceived indicates whether we have received audio data
			// from the websocket. This is so we can raise an exception if we
			// don't receive any audio data.
			audioWasReceived := false

			defer conn.Close()
			defer func() {
				if err := recover(); err != nil {
					fmt.Printf("communicate.Stream recovered from panic: %v stack: %s", err, string(debug.Stack()))
				}
			}()

			for {
				msgType, message, err := conn.ReadMessage()
				if err != nil {
					if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
						// WebSocket error
						output <- map[string]interface{}{
							"error": webSocketError{Message: err.Error()},
						}
					}
					break
				}
				switch msgType {
				case websocket.TextMessage:
					parameters, data := processWebsocketTextMessage(message)
					path := parameters["Path"]

					switch path {
					case "turn.start":
						downloadAudio = true
					case "turn.end":
						output <- map[string]interface{}{
							"end": "",
						}
						downloadAudio = false
						break // End of audio data

					case "audio.metadata":
						metadata, err := processMetadata(data)
						if err != nil {
							output <- map[string]interface{}{
								"error": unknownResponse{Message: err.Error()},
							}
							break
						}

						for _, metaObj := range metadata {
							metaType := metaObj.Type
							if idx != prevIdx {
								shiftTime = sumWithMap(idx, finalUtterance)
								prevIdx = idx
							}
							switch metaType {
							case "WordBoundary":
								finalUtterance[idx] = metaObj.Data.Offset + metaObj.Data.Duration + 8_750_000
								output <- map[string]interface{}{
									"type":     metaType,
									"offset":   metaObj.Data.Offset + shiftTime,
									"duration": metaObj.Data.Duration,
									"text":     metaObj.Data.Text,
								}
							case "SessionEnd":
								// do nothing
							default:
								output <- map[string]interface{}{
									"error": unknownResponse{Message: "Unknown metadata type: " + metaType},
								}
								break
							}
						}
					case "response":
						// do nothing
					default:
						output <- map[string]interface{}{
							"error": unknownResponse{Message: "The response from the service is not recognized.\n" + string(message)},
						}
						break
					}
				case websocket.BinaryMessage:
					if !downloadAudio {
						output <- map[string]interface{}{
							"error": unknownResponse{"We received a binary message, but we are not expecting one."},
						}
					}

					if len(message) < 2 {
						output <- map[string]interface{}{
							"error": unknownResponse{"We received a binary message, but it is missing the header length."},
						}
					}

					headerLength := int(binary.BigEndian.Uint16(message[:2]))
					if len(message) < headerLength+2 {
						output <- map[string]interface{}{
							"error": unknownResponse{"We received a binary message, but it is missing the audio data."},
						}
					}

					audioBinaryData := message[headerLength+2:]
					output <- map[string]interface{}{
						"type": "audio",
						"data": audioData{
							Data:  audioBinaryData,
							Index: idx,
						},
					}
					audioWasReceived = true
				default:
					if message != nil {
						output <- map[string]interface{}{
							"error": webSocketError{
								Message: string(message),
							},
						}
					} else {
						output <- map[string]interface{}{
							"error": webSocketError{
								Message: "Unknown error",
							},
						}
					}
				}

			}
			if !audioWasReceived {
				output <- map[string]interface{}{
					"error": noAudioReceived{Message: "No audio was received. Please verify that your parameters are correct."},
				}
			}
		}(conn, idx)
	}
	c.op = output
	return output, nil
}

func sumWithMap(idx int, m map[int]int) int {
	sumResult := 0
	for i := 0; i < idx; i++ {
		sumResult += m[i]
	}
	return sumResult
}

func setupWebSocketProxy(dialer *websocket.Dialer, c *Communicate) {
	if c.httpProxy != "" {
		proxyUrl, _ := url.Parse(c.httpProxy)
		dialer.Proxy = http.ProxyURL(proxyUrl)
	}
	if c.socket5Proxy != "" {
		auth := &proxy.Auth{User: c.socket5ProxyUser, Password: c.socket5ProxyPass}
		socket5ProxyDialer, _ := proxy.SOCKS5("tcp", c.socket5Proxy, auth, proxy.Direct)
		dialContext := func(ctx context.Context, network, address string) (net.Conn, error) {
			return socket5ProxyDialer.Dial(network, address)
		}
		dialer.NetDialContext = dialContext
	}
}

func processMetadata(data []byte) ([]metaDataEntry, error) {
	var metadata struct {
		Metadata []metaDataEntry `json:"Metadata"`
	}
	err := json.Unmarshal(data, &metadata)
	if err != nil {
		return nil, fmt.Errorf("err=%s, data=%s", err.Error(), string(data))
	}
	return metadata.Metadata, nil
}

func processWebsocketTextMessage(data []byte) (map[string]string, []byte) {
	headers := make(map[string]string)
	headerEndIndex := bytes.Index(data, []byte("\r\n\r\n"))
	headerLines := bytes.Split(data[:headerEndIndex], []byte("\r\n"))

	for _, line := range headerLines {
		header := bytes.SplitN(line, []byte(":"), 2)
		if len(header) == 2 {
			headers[string(bytes.TrimSpace(header[0]))] = string(bytes.TrimSpace(header[1]))
		}
	}

	return headers, data[headerEndIndex+4:]
}

func removeIncompatibleCharacters(str string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\t' && r != '\n' && r != '\r' {
			return ' '
		}
		return r
	}, str)
}

func generateConnectID() string {
	return strings.ReplaceAll(uuid.New().String(), "-", "")
}

// splitTextByByteLength splits the input text into chunks of a specified byte length.
// The function ensures that the text is not split in the middle of a word. If a word exceeds the specified byte length,
// the word is placed in the next chunk. The function returns a slice of byte slices, each representing a chunk of the original text.
//
// Parameters:
// text: The input text to be split.
// byteLength: The maximum byte length for each chunk of text.
//
// Returns:
// A slice of byte slices, each representing a chunk of the original text.
func splitTextByByteLength(text string, byteLength int) [][]byte {
	var result [][]byte
	textBytes := []byte(text)

	if byteLength > 0 {
		for len(textBytes) > byteLength {
			splitAt := bytes.LastIndexByte(textBytes[:byteLength], ' ')
			if splitAt == -1 || splitAt == 0 {
				splitAt = byteLength
			} else {
				splitAt++
			}

			trimmedText := bytes.TrimSpace(textBytes[:splitAt])
			if len(trimmedText) > 0 {
				result = append(result, trimmedText)
			}
			textBytes = textBytes[splitAt:]
		}
	}

	trimmedText := bytes.TrimSpace(textBytes)
	if len(trimmedText) > 0 {
		result = append(result, trimmedText)
	}

	return result
}

func makeSsml(text string, voice string, rate string, volume string) string {
	ssml := fmt.Sprintf(ssmlTemplate,
		voice,
		rate,
		volume,
		text)
	return ssml
}

func currentTimeInMST() string {
	// Use time.FixedZone to represent a fixed timezone offset of 0 (UTC)
	zone := time.FixedZone("UTC", 0)
	now := time.Now().In(zone)
	return now.Format("Mon Jan 02 2006 15:04:05 GMT-0700 (MST)")
}

func ssmlHeadersAppendExtraData(requestID string, timestamp string, ssml string) string {
	headers := fmt.Sprintf(ssmlHeaderTemplate,
		requestID,
		timestamp,
	)
	return headers + ssml
}

func calculateMaxMessageSize(voice string, rate string, volume string) int {
	websocketMaxSize := 1 << 16
	overheadPerMessage := len(ssmlHeadersAppendExtraData(generateConnectID(), currentTimeInMST(), makeSsml("", voice, rate, volume))) + 50
	return websocketMaxSize - overheadPerMessage
}

func escape(data string) string {
	// Must do ampersand first
	entities := make(map[string]string)
	data = html.EscapeString(data)
	data = escapeReplacer.Replace(data)
	if entities != nil {
		data = replaceWithDict(data, entities)
	}
	return data
}

func replaceWithDict(data string, entities map[string]string) string {
	for key, value := range entities {
		data = strings.ReplaceAll(data, key, value)
	}
	return data
}
