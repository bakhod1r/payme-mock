// Package errors defines the Payme protocol error model shared by every
// bounded context: numeric codes, the three-language message object the
// protocol requires, and the mapping from domain failures to wire errors.
package errors

import (
	"errors"
	"fmt"
)

// Code is a Payme JSON-RPC error code.
type Code int

// Transport and JSON-RPC level codes.
const (
	CodeParse          Code = -32700 // Ошибка парсинга JSON
	CodeInvalidRequest Code = -32600 // Отсутствует обязательное поле
	CodeMethodNotFound Code = -32601 // Запрашиваемый метод не найден
	CodeUnauthorized   Code = -32504 // Недостаточно привилегий
	CodeTransport      Code = -32400 // Системная ошибка
	CodeInvalidHTTP    Code = -32300 // Неверный HTTP метод
)

// Merchant API business codes.
const (
	CodeInvalidAmount     Code = -31001 // Неверная сумма
	CodeTransactionNotFnd Code = -31003 // Транзакция не найдена
	CodeOrderCompleted    Code = -31007 // Заказ выполнен, невозможно отменить
	CodeCannotPerform     Code = -31008 // Невозможно выполнить операцию
)

// Account error range. The protocol reserves -31099..-31050 for problems with
// the customer account supplied by the payer; the `data` field must name the
// offending account subfield.
const (
	CodeAccountMin Code = -31099
	CodeAccountMax Code = -31050
)

// IsAccountCode reports whether c lies in the reserved account error range.
func (c Code) IsAccountCode() bool { return c >= CodeAccountMin && c <= CodeAccountMax }

// Message is the localized error text the protocol requires. All three
// languages are mandatory in responses.
type Message struct {
	RU string `json:"ru"`
	UZ string `json:"uz"`
	EN string `json:"en"`
}

// NewMessage builds a Message from its three translations.
func NewMessage(ru, uz, en string) Message { return Message{RU: ru, UZ: uz, EN: en} }

// Localize returns the text for a language tag, falling back to Russian, which
// the protocol treats as the default.
func (m Message) Localize(lang string) string {
	switch lang {
	case "uz":
		return m.UZ
	case "en":
		return m.EN
	default:
		return m.RU
	}
}

// Complete reports whether every required translation is present.
func (m Message) Complete() bool { return m.RU != "" && m.UZ != "" && m.EN != "" }

// ProtocolError is an error that carries a Payme error code and is rendered
// into the JSON-RPC `error` object.
type ProtocolError struct {
	Code    Code
	Message Message
	// Data names the offending field for account errors; it is omitted when empty.
	Data string
}

// New builds a ProtocolError.
func New(code Code, msg Message, data string) *ProtocolError {
	return &ProtocolError{Code: code, Message: msg, Data: data}
}

// Error implements the error interface.
func (e *ProtocolError) Error() string {
	if e.Data != "" {
		return fmt.Sprintf("payme error %d: %s (%s)", e.Code, e.Message.RU, e.Data)
	}
	return fmt.Sprintf("payme error %d: %s", e.Code, e.Message.RU)
}

// Is lets errors.Is match two protocol errors by code.
func (e *ProtocolError) Is(target error) bool {
	var other *ProtocolError
	if !errors.As(target, &other) {
		return false
	}
	return e.Code == other.Code
}

// WithData returns a copy of the error naming the offending account field.
func (e *ProtocolError) WithData(field string) *ProtocolError {
	clone := *e
	clone.Data = field
	return &clone
}

// WithMessage returns a copy of the error carrying different localized text.
func (e *ProtocolError) WithMessage(msg Message) *ProtocolError {
	clone := *e
	clone.Message = msg
	return &clone
}

// As extracts a *ProtocolError from err, reporting whether one was found.
func As(err error) (*ProtocolError, bool) {
	var pe *ProtocolError
	ok := errors.As(err, &pe)
	return pe, ok
}
