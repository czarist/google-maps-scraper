package web

import (
	"net/http"
)

const campaignsPath = "/campaigns"

// serviceCreate registers a sending service from the campaigns page.
func (s *Server) serviceCreate(w http.ResponseWriter, r *http.Request) {
	s.postService(w, r, func() error {
		return s.svc.CreateService(r.Context(), &MailService{
			Name:  r.Form.Get("service_name"),
			Label: r.Form.Get("service_label"),
		})
	}, "Service created")
}

// serviceUpdate changes the label of a service.
func (s *Server) serviceUpdate(w http.ResponseWriter, r *http.Request) {
	s.postService(w, r, func() error {
		return s.svc.UpdateService(r.Context(), &MailService{
			Name:  r.PathValue("name"),
			Label: r.Form.Get("service_label"),
		})
	}, "Service saved")
}

// serviceDelete removes a service no campaign uses.
func (s *Server) serviceDelete(w http.ResponseWriter, r *http.Request) {
	s.postService(w, r, func() error {
		return s.svc.DeleteService(r.Context(), r.PathValue("name"))
	}, "Service deleted")
}

func (s *Server) postService(w http.ResponseWriter, r *http.Request, fn func() error, notice string) {
	if r.Method != http.MethodPost {
		http.Error(w, methodNotAllowedMessage, http.StatusMethodNotAllowed)

		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	if err := fn(); err != nil {
		redirectQuery(w, r, campaignsPath, "error", publicErr(err))

		return
	}

	redirectQuery(w, r, campaignsPath, "notice", notice)
}
