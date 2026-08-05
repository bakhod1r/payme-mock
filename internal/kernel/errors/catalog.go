package errors

// Catalog holds the documented errors with their three-language text. It is the
// seed for the editable database catalog; the UI may override any message, but
// these are the defaults taken from the Payme documentation.
var (
	ErrParse = New(CodeParse, NewMessage(
		"Ошибка парсинга JSON",
		"JSON tahlil qilishda xatolik",
		"JSON parsing error",
	), "")

	ErrInvalidRequest = New(CodeInvalidRequest, NewMessage(
		"Отсутствует обязательное поле",
		"Majburiy maydon yo'q",
		"Required field is missing",
	), "")

	ErrMethodNotFound = New(CodeMethodNotFound, NewMessage(
		"Запрашиваемый метод не найден",
		"So'ralgan metod topilmadi",
		"Requested method was not found",
	), "")

	ErrUnauthorized = New(CodeUnauthorized, NewMessage(
		"Недостаточно привилегий для выполнения метода",
		"Metodni bajarish uchun huquqlar yetarli emas",
		"Insufficient privileges to execute the method",
	), "")

	ErrTransport = New(CodeTransport, NewMessage(
		"Системная ошибка",
		"Tizim xatosi",
		"System error",
	), "")

	ErrInvalidHTTPMethod = New(CodeInvalidHTTP, NewMessage(
		"Неверный HTTP метод",
		"Noto'g'ri HTTP metod",
		"Invalid HTTP method",
	), "")

	ErrInvalidAmount = New(CodeInvalidAmount, NewMessage(
		"Неверная сумма",
		"Noto'g'ri summa",
		"Invalid amount",
	), "")

	ErrTransactionNotFound = New(CodeTransactionNotFnd, NewMessage(
		"Транзакция не найдена",
		"Tranzaksiya topilmadi",
		"Transaction not found",
	), "")

	ErrOrderCompleted = New(CodeOrderCompleted, NewMessage(
		"Заказ выполнен. Невозможно отменить транзакцию",
		"Buyurtma bajarilgan. Tranzaksiyani bekor qilib bo'lmaydi",
		"Order completed. Cannot cancel transaction",
	), "")

	ErrCannotPerform = New(CodeCannotPerform, NewMessage(
		"Невозможно выполнить данную операцию",
		"Ushbu operatsiyani bajarib bo'lmaydi",
		"Unable to perform this operation",
	), "")

	// ErrAccountNotFound is the generic account failure. Callers name the
	// offending field with WithData.
	ErrAccountNotFound = New(CodeAccountMax, NewMessage(
		"Неверный код заказа",
		"Buyurtma kodi noto'g'ri",
		"Invalid order code",
	), "")
)

// Catalog lists every documented error, in the order the documentation
// presents them. Seeding and completeness tests iterate over this slice.
var Catalog = []*ProtocolError{
	ErrParse,
	ErrInvalidRequest,
	ErrMethodNotFound,
	ErrUnauthorized,
	ErrTransport,
	ErrInvalidHTTPMethod,
	ErrInvalidAmount,
	ErrTransactionNotFound,
	ErrOrderCompleted,
	ErrCannotPerform,
	ErrAccountNotFound,
}

// ByCode returns the catalog entry for a code, reporting whether it exists.
func ByCode(code Code) (*ProtocolError, bool) {
	for _, e := range Catalog {
		if e.Code == code {
			return e, true
		}
	}
	return nil, false
}
