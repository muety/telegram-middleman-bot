package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/muety/telepush/config"
	"github.com/muety/telepush/model"
	"github.com/muety/telepush/services"
	"github.com/muety/telepush/store"
	"github.com/muety/telepush/util"
	limiter "github.com/n1try/limiter/v3"
	memst "github.com/n1try/limiter/v3/drivers/store/memory"
	"io/ioutil"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var (
	botStore       store.Store
	botConfig      *config.BotConfig
	client         *http.Client
	cmdRateLimiter *limiter.Limiter
	userService    *services.UserService
)

func init() {
	// get config
	botConfig = config.Get()
	botStore = config.GetStore()

	// init services
	userService = services.NewUserService(botStore)

	// init http client
	client = &http.Client{Timeout: (config.PollTimeoutSec + 10) * time.Second}
	if botConfig.ProxyURI != nil && botConfig.ProxyURI.String() != "" {
		client.Transport = &http.Transport{Proxy: http.ProxyURL(botConfig.ProxyURI)}
	}

	// init rate limiter
	rate, err := limiter.NewRateFromFormatted(fmt.Sprintf("%d-H", botConfig.CmdRateLimit))
	if err != nil {
		log.Fatalln("failed to parse command rate limit string")
	}
	cmdRateLimiter = limiter.New(memst.NewStore(), rate)
}

func GetUpdate() (*[]model.TelegramUpdate, error) {
	offset := 0
	if botStore.Get(config.KeyUpdateID) != nil {
		offset = int(botStore.Get(config.KeyUpdateID).(float64)) + 1
	}
	apiUrl := botConfig.GetApiUrl() + "/getUpdates?timeout=" + strconv.Itoa(config.PollTimeoutSec) + "&offset=" + strconv.Itoa(offset)
	log.Println("polling for updates")
	request, _ := http.NewRequest(http.MethodGet, apiUrl, nil)
	request.Close = true

	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	data, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}

	if response.StatusCode != http.StatusOK {
		return nil, errors.New(string(data))
	}

	var update model.TelegramUpdateResponse
	if err := json.Unmarshal(data, &update); err != nil {
		return nil, err
	}

	if len(update.Result) > 0 {
		var latestUpdateId interface{} = float64(update.Result[len(update.Result)-1].UpdateId)
		botStore.Put(config.KeyUpdateID, latestUpdateId)
	}

	return &update.Result, nil
}

func Poll() {
	go func() {
		for {
			if updates, err := GetUpdate(); err == nil {
				for _, update := range *updates {
					go processUpdate(update)
				}
			} else {
				log.Printf("error getting updates: %s\n", err)
				time.Sleep(config.PollTimeoutSec * time.Second)
			}
		}
	}()
}


func NotFound(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	w.WriteHeader(http.StatusNotFound)
}

func Webhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var u model.TelegramUpdate
	if err := json.NewDecoder(r.Body).Decode(&u); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(err.Error()))
		return
	}

	go processUpdate(u)

	w.WriteHeader(http.StatusAccepted)
}

func SendMessage(message *model.TelegramOutMessage) error {
	buf := bytes.Buffer{}
	if err := json.NewEncoder(&buf).Encode(message); err != nil {
		return err
	}

	request, _ := http.NewRequest(http.MethodPost, botConfig.GetApiUrl()+"/sendMessage", &buf)
	request.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(request)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return handleApiResponse(resp)
}

func SendDocument(document *model.TelegramOutDocument) error {
	buf, contentType, err := document.EncodeMultipart()
	if err != nil {
		return err
	}

	request, _ := http.NewRequest(http.MethodPost, botConfig.GetApiUrl()+"/sendDocument", buf)
	request.Header.Set("Content-Type", contentType)

	resp, err := client.Do(request)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return handleApiResponse(resp)
}

func processUpdate(update model.TelegramUpdate) {
	text := config.MessageDefaultResponse                 // out text
	messageText := strings.TrimSpace(update.Message.Text) // in text
	chatId := update.Message.Chat.Id

	if checkBlacklist(chatId) {
		log.Printf("got update from blacklisted chat id '%d'\n", chatId)
		return
	}

	if !checkWhitelist(chatId) {
		log.Printf("got update not from whitelisted chat id '%d'\n", chatId)
		return
	}

	// check rate limit
	if limitCtx, _ := cmdRateLimiter.Get(context.Background(), fmt.Sprintf("%d-%s", chatId, update.Message.Text)); limitCtx.Reached {
		log.Printf("command rate limit reached for chat '%d'\n", chatId)
		return
	}

	if cmd := config.CmdStart; cmd.MatchString(messageText) {
		// create new token
		token := util.RandomString(6)
		userService.SetToken(token, update.Message.From, chatId)
		text = fmt.Sprintf(config.MessageTokenResponse, token)
		log.Printf("sending new token %s to %d", token, chatId)
	} else if cmd := config.CmdRevoke; cmd.MatchString(messageText) {
		tokens := userService.ListTokens(chatId)

		if matches := cmd.FindStringSubmatch(messageText); len(matches) > 1 && matches[1] != "" {
			if idx, _ := strconv.Atoi(matches[1]); idx > 0 && idx <= len(tokens) {
				userService.InvalidateToken(tokens[idx-1])
				text = fmt.Sprintf(config.MessageRevokeSuccessful, tokens[idx-1])
			} else {
				text = fmt.Sprintf(config.MessageRevokeInvalidIndex, idx)
			}
		} else {
			if len(tokens) == 0 {
				text = config.MessageRevokeListEmpty
			} else {
				text = fmt.Sprintf(config.MessageRevokeList, tokens)
			}
		}
	} else if cmd := config.CmdHelp; cmd.MatchString(messageText) {
		// print help message
		text = fmt.Sprintf(config.MessageHelpResponse, chatId, botConfig.Version)
	} else {
		log.Printf("got unknown command: '%s' from chat '%d'\n", update.Message.Text, chatId)
	}

	if err := SendMessage(&model.TelegramOutMessage{
		ChatId:             strconv.FormatInt(chatId, 10),
		Text:               text,
		ParseMode:          "Markdown",
		DisableLinkPreview: true,
	}); err != nil {
		log.Printf("error responding to update for chat '%d': %v\n", chatId, err)
	}
}

func checkBlacklist(senderId int64) bool {
	for _, id := range botConfig.Blacklist {
		if id == senderId {
			return true
		}
	}
	return false
}

func checkWhitelist(senderId int64) bool {
	// Not in whitelist mode
	if len(botConfig.Whitelist) == 0 {
		return true
	}
	for _, id := range botConfig.Whitelist {
		if id == senderId {
			return true
		}
	}
	return false
}

func handleApiResponse(response *http.Response) error {
	resData, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return err
	}

	var jsonResponse map[string]interface{}
	if err := json.Unmarshal(resData, &jsonResponse); err != nil {
		return err
	} else if ok := jsonResponse["ok"]; !(ok.(bool)) {
		desc := jsonResponse["description"].(string)
		status := jsonResponse["error_code"].(float64)
		return errors.New(fmt.Sprintf("telegram api returned status %d: '%s'\n", int(status), desc))
	}

	return nil
}
