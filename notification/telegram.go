package notification

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	tb "gopkg.in/telebot.v3"

	"github.com/rodrigo-brito/ninjabot/exchange"
	"github.com/rodrigo-brito/ninjabot/model"
	"github.com/rodrigo-brito/ninjabot/order"
	"github.com/rodrigo-brito/ninjabot/service"
)

var (
	buyRegexp  = regexp.MustCompile(`/buy\s+(?P<pair>\w+)\s+(?P<amount>\d+(?:\.\d+)?)(?P<percent>%)?`)
	sellRegexp = regexp.MustCompile(`/sell\s+(?P<pair>\w+)\s+(?P<amount>\d+(?:\.\d+)?)(?P<percent>%)?`)
)

type telegram struct {
	settings        model.Settings
	orderController *order.Controller
	defaultMenu     *tb.ReplyMarkup
	client          *tb.Bot
}

type Option func(telegram *telegram)

func NewTelegram(controller *order.Controller, settings model.Settings, options ...Option) (service.Telegram, error) {
	menu := &tb.ReplyMarkup{ResizeKeyboard: true}
	poller := &tb.LongPoller{Timeout: 10 * time.Second}

	userMiddleware := tb.NewMiddlewarePoller(poller, func(u *tb.Update) bool {
		if u.Message == nil || u.Message.Sender == nil {
			log.Error("no message, ", u)
			return false
		}

		for _, user := range settings.Telegram.Users {
			if int(u.Message.Sender.ID) == user {
				return true
			}
		}

		log.Error("invalid user, ", u.Message)
		return false
	})

	client, err := tb.NewBot(tb.Settings{
		ParseMode: tb.ModeMarkdown,
		Token:     settings.Telegram.Token,
		Poller:    userMiddleware,
	})
	if err != nil {
		return nil, err
	}

	var (
		statusBtn  = menu.Text("/status")
		profitBtn  = menu.Text("/profit")
		balanceBtn = menu.Text("/balance")
		startBtn   = menu.Text("/start")
		stopBtn    = menu.Text("/stop")
		buyBtn     = menu.Text("/buy")
		sellBtn    = menu.Text("/sell")
	)

	err = client.SetCommands([]tb.Command{
		{Text: "/help", Description: "Display help instructions"},
		{Text: "/stop", Description: "Stop buy and sell coins"},
		{Text: "/start", Description: "Start buy and sell coins"},
		{Text: "/status", Description: "Check bot status"},
		{Text: "/balance", Description: "Wallet balance"},
		{Text: "/profit", Description: "Summary of last trade results"},
		{Text: "/buy", Description: "open a buy order"},
		{Text: "/sell", Description: "open a sell order"},
	})
	if err != nil {
		return nil, err
	}

	menu.Reply(
		menu.Row(statusBtn, balanceBtn, profitBtn),
		menu.Row(startBtn, stopBtn, buyBtn, sellBtn),
	)

	bot := &telegram{
		orderController: controller,
		client:          client,
		settings:        settings,
		defaultMenu:     menu,
	}

	for _, option := range options {
		option(bot)
	}

	client.Handle("/help", bot.HelpHandle)
	client.Handle("/start", bot.StartHandle)
	client.Handle("/stop", bot.StopHandle)
	client.Handle("/status", bot.StatusHandle)
	client.Handle("/balance", bot.BalanceHandle)
	client.Handle("/profit", bot.ProfitHandle)
	client.Handle("/buy", bot.BuyHandle)
	client.Handle("/sell", bot.SellHandle)

	return bot, nil
}

func (t telegram) Start() {
	go t.client.Start()
	for _, id := range t.settings.Telegram.Users {
		_, err := t.client.Send(&tb.User{ID: int64(id)}, "Bot initialized.", t.defaultMenu)
		if err != nil {
			log.Error(err)
		}
	}
}

func (t telegram) Notify(text string) {
	for _, user := range t.settings.Telegram.Users {
		_, err := t.client.Send(&tb.User{ID: int64(user)}, text)
		if err != nil {
			log.Error(err)
		}
	}
}

func (t telegram) BalanceHandle(c tb.Context) error {
	message := "*BALANCE*\n"
	quotesValue := make(map[string]float64)
	total := 0.0

	account, err := t.orderController.Account()
	if err != nil {
		log.Error(err)
		t.OnError(err)
		return err
	}

	for _, pair := range t.settings.Pairs {
		assetPair, quotePair := exchange.SplitAssetQuote(pair)
		assetBalance, quoteBalance := account.Balance(assetPair, quotePair)

		assetSize := assetBalance.Free + assetBalance.Lock
		quoteSize := quoteBalance.Free + quoteBalance.Lock

		quote, err := t.orderController.LastQuote(pair)
		if err != nil {
			log.Error(err)
			t.OnError(err)
			return err
		}

		assetValue := assetSize * quote
		quotesValue[quotePair] = quoteSize
		total += assetValue
		message += fmt.Sprintf("%s: `%.4f` ≅ `%.2f` %s \n", assetPair, assetSize, assetValue, quotePair)
	}

	for quote, value := range quotesValue {
		total += value
		message += fmt.Sprintf("%s: `%.4f`\n", quote, value)
	}

	message += fmt.Sprintf("-----\nTotal: `%.4f`\n", total)

	_, err = t.client.Send(c.Sender(), message)
	if err != nil {
		log.Error(err)
	}
	return err
}

func (t telegram) HelpHandle(c tb.Context) error {
	commands, err := t.client.Commands()
	if err != nil {
		log.Error(err)
		t.OnError(err)
		return err
	}

	lines := make([]string, 0, len(commands))
	for _, command := range commands {
		lines = append(lines, fmt.Sprintf("/%s - %s", command.Text, command.Description))
	}

	_, err = t.client.Send(c.Sender(), strings.Join(lines, "\n"))
	if err != nil {
		log.Error(err)
		return err
	}
	return nil
}

func (t telegram) ProfitHandle(c tb.Context) error {
	if len(t.orderController.Results) == 0 {
		_, err := t.client.Send(c.Sender(), "No trades registered.")
		if err != nil {
			log.Error(err)
		}
		return err
	}

	for pair, summary := range t.orderController.Results {
		_, err := t.client.Send(c.Sender(), fmt.Sprintf("*PAIR*: `%s`\n`%s`", pair, summary.String()))
		if err != nil {
			log.Error(err)
		}
	}
	return nil
}

func (t telegram) BuyHandle(c tb.Context) error {
	match := buyRegexp.FindStringSubmatch(c.Message().Text)
	if len(match) == 0 {
		_, err := t.client.Send(c.Sender(), "Invalid command.\nExamples of usage:\n`/buy BTCUSDT 100`\n\n`/buy BTCUSDT 50%`")
		if err != nil {
			log.Error(err)
		}
		return err
	}

	command := make(map[string]string)
	for i, name := range buyRegexp.SubexpNames() {
		if i != 0 && name != "" {
			command[name] = match[i]
		}
	}

	pair := strings.ToUpper(command["pair"])
	amount, err := strconv.ParseFloat(command["amount"], 64)
	if err != nil {
		log.Error(err)
		t.OnError(err)
		return err
	} else if amount <= 0 {
		_, err := t.client.Send(c.Sender(), "Invalid amount")
		if err != nil {
			log.Error(err)
		}
		return err
	}

	if command["percent"] != "" {
		_, quote, err := t.orderController.Position(pair)
		if err != nil {
			log.Error(err)
			t.OnError(err)
			return err
		}

		amount = amount * quote / 100.0
	}

	order, err := t.orderController.CreateOrderMarketQuote(model.SideTypeBuy, pair, amount)
	if err != nil {
		return err
	}
	log.Info("[TELEGRAM]: BUY ORDER CREATED: ", order)
	return nil
}

func (t telegram) SellHandle(c tb.Context) error {
	match := sellRegexp.FindStringSubmatch(c.Message().Text)

	if len(match) == 0 {
		_, err := t.client.Send(c.Sender(), "Invalid command.\nExample of usage:\n`/sell BTCUSDT 100`\n\n`/sell BTCUSDT 50%`")
		if err != nil {
			log.Error(err)
		}
		return err
	}

	command := make(map[string]string)
	for i, name := range sellRegexp.SubexpNames() {
		if i != 0 && name != "" {
			command[name] = match[i]
		}
	}

	pair := strings.ToUpper(command["pair"])
	amount, err := strconv.ParseFloat(command["amount"], 64)
	if err != nil {
		log.Error(err)
		t.OnError(err)
		return err
	} else if amount <= 0 {
		_, err := t.client.Send(c.Sender(), "Invalid amount")
		if err != nil {
			log.Error(err)
		}
		return err
	}

	if command["percent"] != "" {
		asset, _, err := t.orderController.Position(pair)
		if err != nil {
			return err
		}

		amount = amount * asset / 100.0
		order, err := t.orderController.CreateOrderMarket(model.SideTypeSell, pair, amount)
		if err != nil {
			return err
		}
		log.Info("[TELEGRAM]: SELL ORDER CREATED: ", order)
		return nil
	}

	order, err := t.orderController.CreateOrderMarketQuote(model.SideTypeSell, pair, amount)
	if err != nil {
		return err
	}
	log.Info("[TELEGRAM]: SELL ORDER CREATED: ", order)
	return nil
}

func (t telegram) StatusHandle(c tb.Context) error {
	status := t.orderController.Status()
	_, err := t.client.Send(c.Sender(), fmt.Sprintf("Status: `%s`", status))
	if err != nil {
		log.Error(err)
	}
	return err 
}

func (t telegram) StartHandle(c tb.Context) error {
	if t.orderController.Status() == order.StatusRunning {
		_, err := t.client.Send(c.Sender(), "Bot is already running.", t.defaultMenu)
		if err != nil {
			log.Error(err)
		}
		return err
	}

	t.orderController.Start()
	_, err := t.client.Send(c.Sender(), "Bot started.", t.defaultMenu)
	if err != nil {
		log.Error(err)
	}
	return err
}

func (t telegram) StopHandle(c tb.Context) error {
	if t.orderController.Status() == order.StatusStopped {
		_, err := t.client.Send(c.Sender(), "Bot is already stopped.", t.defaultMenu)
		if err != nil {
			log.Error(err)
		}
		return err
	}

	t.orderController.Stop()
	_, err := t.client.Send(c.Sender(), "Bot stopped.", t.defaultMenu)
	if err != nil {
		log.Error(err)
	}
	return err
}

func (t telegram) OnOrder(order model.Order) {
	title := ""
	switch order.Status {
	case model.OrderStatusTypeFilled:
		title = fmt.Sprintf("✅ ORDER FILLED - %s", order.Pair)
	case model.OrderStatusTypeNew:
		title = fmt.Sprintf("🆕 NEW ORDER - %s", order.Pair)
	case model.OrderStatusTypeCanceled, model.OrderStatusTypeRejected:
		title = fmt.Sprintf("❌ ORDER CANCELED / REJECTED - %s", order.Pair)
	}
	message := fmt.Sprintf("%s\n-----\n%s", title, order)
	t.Notify(message)
}

func (t telegram) OnError(err error) {
	title := "🛑 ERROR"

	var orderError *exchange.OrderError
	if errors.As(err, &orderError) {
		message := fmt.Sprintf(`%s
		-----
		Pair: %s
		Quantity: %.4f
		-----
		%s`, title, orderError.Pair, orderError.Quantity, orderError.Err)
		t.Notify(message)
		return
	}

	t.Notify(fmt.Sprintf("%s\n-----\n%s", title, err))
}
