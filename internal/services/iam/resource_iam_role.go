package iam

import (
	"context"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/philips-software/terraform-provider-hsdp/internal/config"
	"github.com/philips-software/terraform-provider-hsdp/internal/tools"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/philips-software/go-hsdp-api/iam"
)

func ResourceIAMRole() *schema.Resource {
	return &schema.Resource{
		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},

		CreateContext: resourceIAMRoleCreate,
		ReadContext:   resourceIAMRoleRead,
		UpdateContext: resourceIAMRoleUpdate,
		DeleteContext: resourceIAMRoleDelete,

		Schema: map[string]*schema.Schema{
			"name": {
				Type:         schema.TypeString,
				Required:     true,
				ValidateFunc: tools.ValidateUpperString,
			},
			"description": {
				Type:             schema.TypeString,
				Optional:         true,
				ForceNew:         true,
				DiffSuppressFunc: tools.SuppressWhenGenerated,
			},
			"managing_organization": {
				Type:     schema.TypeString,
				Required: true,
			},
			"permissions": {
				Type:     schema.TypeSet,
				MaxItems: 100,
				Required: true,
				Elem:     &schema.Schema{Type: schema.TypeString},
			},
			"ticket_protection": {
				Type:     schema.TypeBool,
				Optional: true,
				Default:  true,
			},
			"_id": {
				Type:     schema.TypeString,
				Computed: true,
			},
		},
	}
}

func resourceIAMRoleCreate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	c := meta.(*config.Config)

	var diags diag.Diagnostics

	client, err := c.IAMClient()
	if err != nil {
		return diag.FromErr(err)
	}

	name := d.Get("name").(string)
	description := d.Get("description").(string)
	managingOrganization := d.Get("managing_organization").(string)
	permissions := tools.ExpandStringList(d.Get("permissions").(*schema.Set).List())

	var role *iam.Role
	var resp *iam.Response

	err = tools.TryIAMCall(func() (*iam.Response, error) {
		var err error
		role, resp, err = client.Roles.CreateRole(name, description, managingOrganization)
		if err != nil {
			_ = client.TokenRefresh()
		}
		return resp, err
	})
	if err != nil {
		if resp == nil {
			return diag.FromErr(fmt.Errorf("response is nil: %v", err))
		}
		if resp.StatusCode != http.StatusConflict {
			return diag.FromErr(err)
		}
		// Already exists most likely, adopt it
		var roles *[]iam.Role
		roles, _, err = client.Roles.GetRoles(&iam.GetRolesOptions{
			Name:           &name,
			OrganizationID: &managingOrganization,
		})
		if err != nil {
			return diag.FromErr(err)
		}
		if len(*roles) == 0 || (*roles)[0].ManagingOrganization != managingOrganization {
			return diag.FromErr(fmt.Errorf("conflict creating role, mismatched managing_organization: '%s' != '%s'",
				(*roles)[0].ManagingOrganization, managingOrganization))
		}
		role = &(*roles)[0]
	}
	for _, p := range permissions {
		_, _, _ = client.Roles.AddRolePermission(*role, p)
	}
	d.SetId(role.ID)
	readDiags := resourceIAMRoleRead(ctx, d, meta)
	if readDiags != nil {
		diags = append(diags, readDiags...)
	}
	return diags
}

func resourceIAMRoleRead(_ context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	c := meta.(*config.Config)

	var diags diag.Diagnostics

	client, err := c.IAMClient()
	if err != nil {
		return diag.FromErr(err)
	}

	id := d.Id()
	role, resp, err := client.Roles.GetRoleByID(id)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			d.SetId("")
			return diags
		}
		return diag.FromErr(err)
	}
	_ = d.Set("description", role.Description)
	_ = d.Set("name", role.Name)
	_ = d.Set("managing_organization", role.ManagingOrganization)

	permissions, resp, err := client.Roles.GetRolePermissions(*role)
	if err != nil {
		if resp.StatusCode == http.StatusForbidden { // IAM limitation
			return diags // Use Terraform as source of truth
		}
		return diag.FromErr(err)
	}
	_ = d.Set("permissions", permissions)
	return diags
}

func resourceIAMRoleUpdate(_ context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	c := m.(*config.Config)

	var diags diag.Diagnostics

	client, err := c.IAMClient()
	if err != nil {
		return diag.FromErr(err)
	}

	id := d.Id()
	role, _, err := client.Roles.GetRoleByID(id)
	if err != nil {
		return diag.FromErr(err)
	}

	if d.HasChange("description") {
		return diag.FromErr(fmt.Errorf("description changes are not supported"))
	}

	if d.HasChange("permissions") {
		o, n := d.GetChange("permissions")
		oldList := tools.ExpandStringList(o.(*schema.Set).List())
		newList := tools.ExpandStringList(n.(*schema.Set).List())
		toAdd := tools.Difference(newList, oldList)
		toRemove := tools.Difference(oldList, newList)

		// Additions
		if len(toAdd) > 0 {
			for _, v := range toAdd {
				_, _, err := client.Roles.AddRolePermission(*role, v)
				if err != nil {
					return diag.FromErr(err)
				}
			}
		}

		// Removals
		for _, v := range toRemove {
			ticketProtection := d.Get("ticket_protection").(bool)
			if ticketProtection && v == "CLIENT.SCOPES" {
				return diag.FromErr(fmt.Errorf("Refusing to remove CLIENT.SCOPES permission, set ticket_protection to `false` to override"))
			}
			_, _, err := client.Roles.RemoveRolePermission(*role, v)
			if err != nil {
				return diag.FromErr(err)
			}
		}

	}
	return diags
}

func resourceIAMRoleDelete(_ context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	c := m.(*config.Config)

	var diags diag.Diagnostics

	client, err := c.IAMClient()
	if err != nil {
		return diag.FromErr(err)
	}

	var role iam.Role
	role.ID = d.Id()

	_, _, err = client.Roles.DeleteRole(role)
	if err != nil {
		return diag.FromErr(err)
	}
	d.SetId("")
	return diags
}
